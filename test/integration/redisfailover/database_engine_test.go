//go:build integration
// +build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/homedir"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/cmd/utils"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	"github.com/freshworks/redis-operator/operator/redisfailover"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
)

const (
	valkeyIntegrationNamespace = "testns-valkey"
	valkeyIntegrationRFName    = "testing-valkey"
)

// TestRedisFailoverValkeyDatabaseEngine exercises spec.engine=Valkey end-to-end against a real cluster:
// generated workloads use valkey-server / valkey-cli, auth and replication behave like the default Redis test.
func TestRedisFailoverValkeyDatabaseEngine(t *testing.T) {
	require := require.New(t)
	disableMyMaster := true
	currentNamespace := valkeyIntegrationNamespace
	rfName := valkeyIntegrationRFName

	stopC := make(chan struct{})
	errC := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())

	flags := &utils.CMDFlags{
		KubeConfig:  filepath.Join(homedir.HomeDir(), ".kube", "config"),
		Development: true,
	}

	k8sClient, customClient, aeClientset, cmClient, err := utils.CreateKubernetesClients(flags)
	require.NoError(err)

	redisClient := redis.New(metrics.Dummy)

	clients := clients{
		k8sClient:   k8sClient,
		rfClient:    customClient,
		aeClient:    aeClientset,
		redisClient: redisClient,
	}

	k8sservice := k8s.New(k8sClient, customClient, aeClientset, cmClient, log.Dummy, metrics.Dummy)

	prepErr := clients.prepareNS(currentNamespace)
	require.NoError(prepErr)

	time.Sleep(15 * time.Second)

	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{OperatorGroupID: integrationTestGroupID}, k8sservice, k8sClient, currentNamespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	go func() {
		errC <- redisfailoverOperator.Run(ctx)
	}()

	defer cancel()
	defer clients.cleanup(stopC, currentNamespace)

	time.Sleep(15 * time.Second)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authSecretPath,
			Namespace: currentNamespace,
		},
		Data: map[string][]byte{
			"password": []byte(testPass),
		},
	}
	_, err = k8sClient.CoreV1().Secrets(currentNamespace).Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(err)

	ok := t.Run("Check Custom Resource Creation (Valkey)", func(t *testing.T) {
		clients.testValkeyCRCreation(t, currentNamespace, rfName, disableMyMaster)
	})
	require.True(ok, "the Valkey custom resource has to be created to continue")

	time.Sleep(3 * time.Minute)

	t.Run("Valkey StatefulSet and Sentinel use valkey-server and valkey-cli", func(t *testing.T) {
		clients.testValkeyWorkloadBinaries(t, currentNamespace, rfName)
	})

	t.Run("Check that auth is set in sentinel and redis configs (Valkey)", func(t *testing.T) {
		clients.testAuth(t, currentNamespace, rfName)
	})

	t.Run("Check that custom config behaves as expected (Valkey)", func(t *testing.T) {
		clients.testCustomConfig(t, currentNamespace, rfName)
	})

	t.Run("Check Redis StatefulSet existing and size (Valkey)", func(t *testing.T) {
		clients.testRedisStatefulSet(t, currentNamespace, rfName)
	})

	t.Run("Check Sentinel Deployment existing and size (Valkey)", func(t *testing.T) {
		clients.testSentinelDeployment(t, currentNamespace, rfName)
	})

	t.Run("Check Only One Redis Master (Valkey)", func(t *testing.T) {
		clients.testRedisMaster(t, currentNamespace, rfName)
	})

	t.Run("Check Sentinels monitoring Redis Master (Valkey)", func(t *testing.T) {
		clients.testSentinelMonitoring(t, currentNamespace, disableMyMaster, rfName)
	})
}

func (c *clients) testValkeyCRCreation(t *testing.T, currentNamespace, rfName string, disableMyMaster bool) {
	assert := assert.New(t)
	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rfName,
			Namespace: currentNamespace,
			Labels:    map[string]string{rfOperatorGroupLabelKey: integrationTestGroupID},
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Engine: redisfailoverv1.ValkeyEngine,
			Redis: redisfailoverv1.RedisSettings{
				Replicas: redisSize,
				Exporter: redisfailoverv1.Exporter{
					Enabled: true,
				},
				CustomConfig: []string{`save ""`},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas:        sentinelSize,
				DisableMyMaster: disableMyMaster,
			},
			Auth: redisfailoverv1.AuthSettings{
				SecretPath: authSecretPath,
			},
		},
	}

	_, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	assert.NoError(err)
	gotRF, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), rfName, metav1.GetOptions{})
	assert.NoError(err)
	assert.Equal(toCreate.Spec, gotRF.Spec)
}

func (c *clients) testValkeyWorkloadBinaries(t *testing.T, currentNamespace, rfName string) {
	assert := assert.New(t)
	require := require.New(t)

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rfName), metav1.GetOptions{})
	require.NoError(err)
	require.NotNil(redisSS.Spec.Template.Spec.Containers[0].Command)
	assert.Equal("valkey-server", redisSS.Spec.Template.Spec.Containers[0].Command[0])
	require.NotNil(redisSS.Spec.Template.Spec.Containers[0].LivenessProbe)
	require.NotNil(redisSS.Spec.Template.Spec.Containers[0].LivenessProbe.Exec)
	assert.Len(redisSS.Spec.Template.Spec.Containers[0].LivenessProbe.Exec.Command, 3)
	assert.Contains(redisSS.Spec.Template.Spec.Containers[0].LivenessProbe.Exec.Command[2], "valkey-cli")

	sentinelD, err := c.k8sClient.AppsV1().Deployments(currentNamespace).Get(context.Background(), fmt.Sprintf("rfs-%s", rfName), metav1.GetOptions{})
	require.NoError(err)
	require.NotNil(sentinelD.Spec.Template.Spec.Containers[0].Command)
	assert.Equal("valkey-server", sentinelD.Spec.Template.Spec.Containers[0].Command[0])
	require.NotNil(sentinelD.Spec.Template.Spec.Containers[0].LivenessProbe.Exec)
	assert.Contains(sentinelD.Spec.Template.Spec.Containers[0].LivenessProbe.Exec.Command[2], "valkey-cli")
	require.NotNil(sentinelD.Spec.Template.Spec.Containers[0].ReadinessProbe.Exec)
	assert.Contains(sentinelD.Spec.Template.Spec.Containers[0].ReadinessProbe.Exec.Command[2], "valkey-cli")

	shutdownCM, err := c.k8sClient.CoreV1().ConfigMaps(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-s-%s", rfName), metav1.GetOptions{})
	require.NoError(err)
	assert.Contains(shutdownCM.Data["shutdown.sh"], "valkey-cli")
	assert.Contains(shutdownCM.Data["shutdown.sh"], "VALKEYCLI_AUTH")

	readinessCM, err := c.k8sClient.CoreV1().ConfigMaps(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-readiness-%s", rfName), metav1.GetOptions{})
	require.NoError(err)
	assert.Contains(readinessCM.Data["ready.sh"], "valkey-cli")
	assert.Contains(readinessCM.Data["ready.sh"], "VALKEYCLI_AUTH")
}
