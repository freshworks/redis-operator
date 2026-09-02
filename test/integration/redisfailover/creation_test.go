//go:build integration
// +build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/util/homedir"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	redisfailoverclientset "github.com/freshworks/redis-operator/client/k8s/clientset/versioned"
	"github.com/freshworks/redis-operator/cmd/utils"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	"github.com/freshworks/redis-operator/operator/redisfailover"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
)

const (
	name                    = "testing"
	namespace               = "testns"
	redisSize               = int32(3)
	sentinelSize            = int32(3)
	authSecretPath          = "redis-auth"
	testPass                = "test-pass"
	redisAddr               = "redis://127.0.0.1:6379"
	integrationTestGroupID  = "integration-test"
	rfOperatorGroupLabelKey = "redis-failover.freshworks.com/operator-group"
)

type clients struct {
	k8sClient   kubernetes.Interface
	rfClient    redisfailoverclientset.Interface
	aeClient    apiextensionsclientset.Interface
	redisClient redis.Client
}

func (c *clients) prepareNS(currentNamespace string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: currentNamespace,
		},
	}
	_, err := c.k8sClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	return err
}

func (c *clients) cleanup(stopC chan struct{}, currentNamespace string) {
	// Signal the operator to stop
	close(stopC)
	// Give the operator time to shut down gracefully
	time.Sleep(5 * time.Second)
	// Delete the namespace
	c.k8sClient.CoreV1().Namespaces().Delete(context.Background(), currentNamespace, metav1.DeleteOptions{})
}

func TestRedisFailover(t *testing.T) {
	require := require.New(t)
	disableMyMaster := true
	currentNamespace := namespace

	// Create signal channels.
	stopC := make(chan struct{})
	errC := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())

	flags := &utils.CMDFlags{
		KubeConfig:  filepath.Join(homedir.HomeDir(), ".kube", "config"),
		Development: true,
	}

	// Kubernetes clients.
	k8sClient, customClient, aeClientset, cmClient, err := utils.CreateKubernetesClients(flags)
	require.NoError(err)

	// Create the redis clients
	redisClient := redis.New(metrics.Dummy)

	clients := clients{
		k8sClient:   k8sClient,
		rfClient:    customClient,
		aeClient:    aeClientset,
		redisClient: redisClient,
	}

	// Create kubernetes service.
	k8sservice := k8s.New(k8sClient, customClient, aeClientset, cmClient, log.Dummy, metrics.Dummy)

	// Prepare namespace
	prepErr := clients.prepareNS(currentNamespace)
	require.NoError(prepErr)

	// Give time to the namespace to be ready
	time.Sleep(15 * time.Second)

	// Create operator and run (label-grouped; tests use integrationTestGroupID).
	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{OperatorGroupID: integrationTestGroupID}, k8sservice, k8sClient, currentNamespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	go func() {
		errC <- redisfailoverOperator.Run(ctx)
	}()

	// Prepare cleanup for when the test ends
	defer cancel()
	defer clients.cleanup(stopC, currentNamespace)

	// Give time to the operator to start
	time.Sleep(15 * time.Second)

	// Create secret
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

	// Check that if we create a RedisFailover, it is certainly created and we can get it
	ok := t.Run("Check Custom Resource Creation", func(t *testing.T) {
		clients.testCRCreation(t, currentNamespace, disableMyMaster)
	})
	require.True(ok, "the custom resource has to be created to continue")

	// Giving time to the operator to create the resources
	time.Sleep(3 * time.Minute)

	// Verify that auth is set and actually working
	t.Run("Check that auth is set in sentinel and redis configs", func(t *testing.T) {
		clients.testAuth(t, currentNamespace)
	})

	// Check custom config is set
	t.Run("Check that custom config is behave expected", func(t *testing.T) {
		clients.testCustomConfig(t, currentNamespace)
	})

	// Check that a Redis Statefulset is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Redis Statefulset existing and size", func(t *testing.T) {
		clients.testRedisStatefulSet(t, currentNamespace)
	})

	// Check that a Sentinel Deployment is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Sentinel Deployment existing and size", func(t *testing.T) {
		clients.testSentinelDeployment(t, currentNamespace)
	})

	// Connect to all the Redis pods and, asking to the Redis running inside them, check
	// that only one of them is the master of the failover.
	t.Run("Check Only One Redis Master", func(t *testing.T) {
		clients.testRedisMaster(t, currentNamespace)
	})

	// Connect to all the Sentinel pods and, asking to the Sentinel running inside them,
	// check that all of them are connected to the same Redis node, and also that that node
	// is the master.
	t.Run("Check Sentinels Checking the Redis Master", func(t *testing.T) {
		clients.testSentinelMonitoring(t, currentNamespace, disableMyMaster)
	})

	// Check that skip reconcile annotation works as expected
	t.Run("Check Skip Reconcile annotation", func(t *testing.T) {
		clients.testSkipReconcile(t, currentNamespace)
	})

	// Check that preventMasterEviction annotations are correctly set
	t.Run("Check PreventMasterEviction annotations", func(t *testing.T) {
		clients.testPreventMasterEviction(t, currentNamespace)
	})

	// Check that maxmemory validation prevents StatefulSet creation when maxmemory exceeds pod memory
	t.Run("Check MaxMemory Validation Error", func(t *testing.T) {
		clients.testMaxMemoryValidationError(t, currentNamespace)
	})

	// Check that maxmemory is not processed via customconfig if maxmemory validation fails when maxmemory exceeds pod memory
	t.Run("Check MaxMemory Healing Validation", func(t *testing.T) {
		clients.testMaxMemoryHealingValidation(t, currentNamespace)
	})

}

func TestRedisFailoverMyMaster(t *testing.T) {
	require := require.New(t)
	disableMyMaster := false
	currentNamespace := "mymaster-" + namespace

	// Create signal channels.
	stopC := make(chan struct{})
	errC := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())

	flags := &utils.CMDFlags{
		KubeConfig:  filepath.Join(homedir.HomeDir(), ".kube", "config"),
		Development: true,
	}

	// Kubernetes clients.
	k8sClient, customClient, aeClientset, cmClient, err := utils.CreateKubernetesClients(flags)
	require.NoError(err)

	// Create the redis clients
	redisClient := redis.New(metrics.Dummy)

	clients := clients{
		k8sClient:   k8sClient,
		rfClient:    customClient,
		aeClient:    aeClientset,
		redisClient: redisClient,
	}

	// Create kubernetes service.
	k8sservice := k8s.New(k8sClient, customClient, aeClientset, cmClient, log.Dummy, metrics.Dummy)

	// Prepare namespace
	prepErr := clients.prepareNS(currentNamespace)
	require.NoError(prepErr)

	// Give time to the namespace to be ready
	time.Sleep(15 * time.Second)

	// Create operator and run (label-grouped; tests use integrationTestGroupID).
	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{OperatorGroupID: integrationTestGroupID}, k8sservice, k8sClient, currentNamespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	go func() {
		errC <- redisfailoverOperator.Run(ctx)
	}()

	// Prepare cleanup for when the test ends
	defer cancel()
	defer clients.cleanup(stopC, currentNamespace)

	// Give time to the operator to start
	time.Sleep(15 * time.Second)

	// Create secret
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

	// Check that if we create a RedisFailover, it is certainly created and we can get it
	ok := t.Run("Check Custom Resource Creation", func(t *testing.T) {
		clients.testCRCreation(t, currentNamespace, disableMyMaster)
	})
	require.True(ok, "the custom resource has to be created to continue")

	// Giving time to the operator to create the resources
	time.Sleep(3 * time.Minute)

	// Verify that auth is set and actually working
	t.Run("Check that auth is set in sentinel and redis configs", func(t *testing.T) {
		clients.testAuth(t, currentNamespace)
	})

	// Check custom config is set
	t.Run("Check that custom config is behave expected", func(t *testing.T) {
		clients.testCustomConfig(t, currentNamespace)
	})

	// Check that a Redis Statefulset is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Redis Statefulset existing and size", func(t *testing.T) {
		clients.testRedisStatefulSet(t, currentNamespace)
	})

	// Check that a Sentinel Deployment is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Sentinel Deployment existing and size", func(t *testing.T) {
		clients.testSentinelDeployment(t, currentNamespace)
	})

	// Connect to all the Redis pods and, asking to the Redis running inside them, check
	// that only one of them is the master of the failover.
	t.Run("Check Only One Redis Master", func(t *testing.T) {
		clients.testRedisMaster(t, currentNamespace)
	})

	// Connect to all the Sentinel pods and, asking to the Sentinel running inside them,
	// check that all of them are connected to the same Redis node, and also that that node
	// is the master.
	t.Run("Check Sentinels Checking the Redis Master", func(t *testing.T) {
		clients.testSentinelMonitoring(t, currentNamespace, disableMyMaster)
	})
}

func (c *clients) testCRCreation(t *testing.T, currentNamespace string, args ...bool) {
	disableMyMaster := false
	if len(args) > 0 && args[0] {
		disableMyMaster = args[0]
	}
	assert := assert.New(t)
	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: currentNamespace,
			Labels:    map[string]string{rfOperatorGroupLabelKey: integrationTestGroupID},
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
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

	c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	gotRF, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), name, metav1.GetOptions{})

	assert.NoError(err)
	assert.Equal(toCreate.Spec, gotRF.Spec)
}

func redisFailoverName(rfName ...string) string {
	if len(rfName) > 0 && rfName[0] != "" {
		return rfName[0]
	}
	return name
}

func (c *clients) testRedisStatefulSet(t *testing.T, currentNamespace string, rfName ...string) {
	assert := assert.New(t)
	rn := redisFailoverName(rfName...)
	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rn), metav1.GetOptions{})
	assert.NoError(err)
	assert.Equal(redisSize, int32(redisSS.Status.Replicas))
}

func (c *clients) testSentinelDeployment(t *testing.T, currentNamespace string, rfName ...string) {
	assert := assert.New(t)
	rn := redisFailoverName(rfName...)
	sentinelD, err := c.k8sClient.AppsV1().Deployments(currentNamespace).Get(context.Background(), fmt.Sprintf("rfs-%s", rn), metav1.GetOptions{})
	assert.NoError(err)
	assert.Equal(3, int(sentinelD.Status.Replicas))
}

func (c *clients) testRedisMaster(t *testing.T, currentNamespace string, rfName ...string) {
	assert := assert.New(t)
	rn := redisFailoverName(rfName...)
	masters := []string{}

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rn), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}

	redisPodList, err := c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)

	assert.NoError(err)

	for _, pod := range redisPodList.Items {
		ip := pod.Status.PodIP
		if ok, _ := c.redisClient.IsMaster(ip, "6379", testPass, nil); ok {
			masters = append(masters, ip)
		}
	}

	assert.Equal(1, len(masters), "only one master expected")
}

func (c *clients) testSentinelMonitoring(t *testing.T, currentNamespace string, disableMyMaster bool, rfName ...string) {
	rn := redisFailoverName(rfName...)
	masterName := "mymaster"
	if disableMyMaster {
		masterName = rn
	}

	assert := assert.New(t)
	masters := []string{}

	sentinelD, err := c.k8sClient.AppsV1().Deployments(currentNamespace).Get(context.Background(), fmt.Sprintf("rfs-%s", rn), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(sentinelD.Spec.Selector.MatchLabels),
	}
	sentinelPodList, err := c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)
	assert.NoError(err)

	for _, pod := range sentinelPodList.Items {
		ip := pod.Status.PodIP
		master, _, _ := c.redisClient.GetSentinelMonitor(ip, masterName, nil)
		masters = append(masters, master)
	}

	for _, masterIP := range masters {
		assert.Equal(masters[0], masterIP, "all master ip monitoring should equal")
	}

	isMaster, err := c.redisClient.IsMaster(masters[0], "6379", testPass, nil)
	assert.NoError(err)
	assert.True(isMaster, "Sentinel should monitor the Redis master")
}

func (c *clients) testAuth(t *testing.T, currentNamespace string, rfName ...string) {
	assert := assert.New(t)
	rn := redisFailoverName(rfName...)

	redisCfg, err := c.k8sClient.CoreV1().ConfigMaps(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rn), metav1.GetOptions{})
	assert.NoError(err)
	assert.Contains(redisCfg.Data["redis.conf"], "requirepass "+testPass)
	assert.Contains(redisCfg.Data["redis.conf"], "masterauth "+testPass)

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rn), metav1.GetOptions{})
	assert.NoError(err)

	assert.Len(redisSS.Spec.Template.Spec.Containers, 2)
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[1].Name, "REDIS_ADDR")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[1].Value, redisAddr)
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[2].Name, "REDIS_PORT")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[2].Value, "6379")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[3].Name, "REDIS_USER")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[3].Value, "default")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].Name, "REDIS_PASSWORD")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].ValueFrom.SecretKeyRef.Key, "password")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].ValueFrom.SecretKeyRef.LocalObjectReference.Name, authSecretPath)
}

func (c *clients) testCustomConfig(t *testing.T, currentNamespace string, rfName ...string) {
	assert := assert.New(t)
	rn := redisFailoverName(rfName...)

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rn), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}
	redisPodList, err := c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)
	assert.NoError(err)

	rClient := rediscli.NewClient(&rediscli.Options{
		Addr:     net.JoinHostPort(redisPodList.Items[0].Status.PodIP, "6379"),
		Password: testPass,
		DB:       0,
	})
	defer rClient.Close()

	result := rClient.ConfigGet(context.TODO(), "save")
	assert.NoError(result.Err())

	values, err := result.Result()
	assert.NoError(err)

	assert.Len(values, 2)
	assert.Empty(values[1])
}

func (c *clients) testSkipReconcile(t *testing.T, currentNamespace string) {
	assert := assert.New(t)
	require := require.New(t)

	// Get the RF
	rf, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(err)

	originalReplicas := rf.Spec.Redis.Replicas

	// Add the skip-reconcile annotation
	rf.Annotations = map[string]string{
		"redis-failover.freshworks.com/skip-reconcile": "true",
	}
	rf, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)
	assert.Equal("true", rf.Annotations["redis-failover.freshworks.com/skip-reconcile"])

	// Update the replicas
	rf.Spec.Redis.Replicas = originalReplicas + 1
	_, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)

	// Giving time to the operator to reconcile
	time.Sleep(30 * time.Second)

	// Check the replicas are not updated
	replicas, err := c.getRedisReplicas(name, currentNamespace)
	require.NoError(err)
	assert.Equal(originalReplicas, replicas)

	// Remove the skip-reconcile annotation
	rf, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(err)
	rf.Annotations = map[string]string{}
	_, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)

	// Giving time to the operator to create the resources
	time.Sleep(30 * time.Second)

	// Check the replicas are updated
	replicas, err = c.getRedisReplicas(name, currentNamespace)
	require.NoError(err)
	assert.Equal(originalReplicas+1, replicas)
}

func (c *clients) testPreventMasterEviction(t *testing.T, currentNamespace string) {
	assert := assert.New(t)
	require := require.New(t)

	// Get the current RedisFailover
	rf, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(err)

	// Enable preventMasterEviction
	rf.Spec.Redis.PreventMasterEviction = true
	rf, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)

	// Give time for the operator to reconcile and update pod annotations
	time.Sleep(35 * time.Second)

	// Get the Redis StatefulSet to use its match labels
	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	require.NoError(err)

	// Get all Redis pods using the StatefulSet's match labels
	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}
	redisPods, err := c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)
	require.NoError(err)
	require.True(len(redisPods.Items) > 0, "should have Redis pods")

	// Check that cluster autoscaler annotations are set correctly
	masterFound := false
	slaveFound := false

	for _, pod := range redisPods.Items {
		// Check if pod has cluster autoscaler annotation
		annotation, exists := pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]
		require.True(exists, "Pod %s should have cluster-autoscaler annotation", pod.Name)

		// Determine if this is master or slave based on role label
		roleLabel, hasRole := pod.Labels["redisfailovers-role"]
		require.True(hasRole, "Pod %s should have role label", pod.Name)

		switch roleLabel {
		case "master":
			assert.Equal("false", annotation, "Master pod %s should have safe-to-evict=false", pod.Name)
			masterFound = true
		case "slave":
			assert.Equal("true", annotation, "Slave pod %s should have safe-to-evict=true", pod.Name)
			slaveFound = true
		}
	}

	// Ensure we found both master and slave pods
	assert.True(masterFound, "Should have found at least one master pod")
	assert.True(slaveFound, "Should have found at least one slave pod")

	// Disable preventMasterEviction and verify annotations are removed
	rf, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(err)
	rf.Spec.Redis.PreventMasterEviction = false
	_, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)

	// Give time for the operator to reconcile and remove annotations
	time.Sleep(35 * time.Second)

	// Verify annotations are removed when preventMasterEviction is disabled
	redisPods, err = c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)
	require.NoError(err)

	for _, pod := range redisPods.Items {
		// When preventMasterEviction is disabled, the cluster autoscaler annotation should be removed
		_, exists := pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]
		assert.False(exists, "Pod %s should not have cluster-autoscaler annotation when preventMasterEviction is disabled", pod.Name)
	}
}

func (c *clients) getRedisReplicas(name string, currentNamespace string) (int32, error) {
	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	return *redisSS.Spec.Replicas, nil
}
func (c *clients) testMaxMemoryValidationError(t *testing.T, currentNamespace string) {
	assert := assert.New(t)
	require := require.New(t)

	// Create a RedisFailover with maxmemory > pod memory to trigger validation error
	rfName := "maxmemory-test"
	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rfName,
			Namespace: currentNamespace,
			Labels:    map[string]string{rfOperatorGroupLabelKey: integrationTestGroupID},
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: 1,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("100Mi"), // Pod memory limit: 100Mi
					},
				},
				CustomConfig: []string{
					"maxmemory 200mb", // maxmemory > pod memory, should fail validation
				},
				Exporter: redisfailoverv1.Exporter{
					Enabled: true,
				},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas: 1,
			},
			Auth: redisfailoverv1.AuthSettings{
				SecretPath: authSecretPath,
			},
		},
	}

	// Create the RedisFailover CRD
	_, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	require.NoError(err)

	// Wait for the operator to attempt processing
	time.Sleep(30 * time.Second)

	// Check that the StatefulSet was NOT created due to validation error
	_, err = c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rfName), metav1.GetOptions{})
	assert.True(errors.IsNotFound(err), "StatefulSet should not be created when maxmemory validation fails")

	// Cleanup: Delete the RedisFailover
	err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Delete(context.Background(), rfName, metav1.DeleteOptions{})
	require.NoError(err)
}
func (c *clients) testMaxMemoryHealingValidation(t *testing.T, currentNamespace string) {
	assert := assert.New(t)
	require := require.New(t)

	// Create a RedisFailover with valid configuration first
	rfName := "maxmemory-healing-test"
	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rfName,
			Namespace: currentNamespace,
			Labels:    map[string]string{rfOperatorGroupLabelKey: integrationTestGroupID},
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: 1,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("200Mi"), // Pod memory limit: 200Mi
					},
				},
				CustomConfig: []string{
					"maxmemory 50mb",               // Valid maxmemory (within limits)
					"maxmemory-policy allkeys-lru", // Other config that should be applied
				},
				Exporter: redisfailoverv1.Exporter{
					Enabled: true,
				},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas: 1,
			},
			Auth: redisfailoverv1.AuthSettings{
				SecretPath: authSecretPath,
			},
		},
	}

	// Create the RedisFailover CRD
	_, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	require.NoError(err)

	// Wait for the operator to create resources
	time.Sleep(40 * time.Second)

	// Verify StatefulSet was created successfully
	redisSS, err := c.k8sClient.AppsV1().StatefulSets(currentNamespace).Get(context.Background(), fmt.Sprintf("rfr-%s", rfName), metav1.GetOptions{})
	require.NoError(err)
	assert.Equal(int32(1), *redisSS.Spec.Replicas)

	// Wait for pods to be ready
	time.Sleep(40 * time.Second)

	// Get Redis pod to verify initial configuration
	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}
	redisPods, err := c.k8sClient.CoreV1().Pods(currentNamespace).List(context.Background(), listOptions)
	require.NoError(err)
	require.True(len(redisPods.Items) > 0, "should have Redis pods")

	// Connect to Redis and verify initial maxmemory configuration
	redisPod := redisPods.Items[0]
	rClient := rediscli.NewClient(&rediscli.Options{
		Addr:     net.JoinHostPort(redisPod.Status.PodIP, "6379"),
		Password: testPass,
		DB:       0,
	})
	defer rClient.Close()

	// Verify initial maxmemory is set correctly
	result := rClient.ConfigGet(context.TODO(), "maxmemory")
	require.NoError(result.Err())
	values, err := result.Result()
	require.NoError(err)
	require.Len(values, 2)
	assert.NotEqual("0", values[1], "maxmemory should be set initially")

	// Verify maxmemory-policy is set
	policyResult := rClient.ConfigGet(context.TODO(), "maxmemory-policy")
	require.NoError(policyResult.Err())
	policyValues, err := policyResult.Result()
	require.NoError(err)
	require.Len(policyValues, 2)
	assert.Equal("allkeys-lru", policyValues[1])

	// Now update the RedisFailover with invalid maxmemory (exceeds pod memory)
	rf, err := c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Get(context.Background(), rfName, metav1.GetOptions{})
	require.NoError(err)

	rf.Spec.Redis.CustomConfig = []string{
		"maxmemory 300mb",              // Invalid maxmemory (exceeds 200Mi pod limit)
		"maxmemory-policy allkeys-lfu", // Valid config that should still be applied
		"tcp-keepalive 120",            // Another valid config
	}

	_, err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Update(context.Background(), rf, metav1.UpdateOptions{})
	require.NoError(err)

	// Wait for healing process to attempt applying the configuration
	time.Sleep(45 * time.Second)

	// Verify that the invalid maxmemory was NOT applied (should remain the old value or be unset)
	// but other valid configurations were applied
	newResult := rClient.ConfigGet(context.TODO(), "maxmemory")
	require.NoError(newResult.Err())
	newValues, err := newResult.Result()
	require.NoError(err)
	require.Len(newValues, 2)
	// The maxmemory should not be the invalid 300mb value
	assert.NotEqual("314572800", newValues[1], "invalid maxmemory should not be applied")

	// Verify that valid configurations were still applied
	newPolicyResult := rClient.ConfigGet(context.TODO(), "maxmemory-policy")
	require.NoError(newPolicyResult.Err())
	newPolicyValues, err := newPolicyResult.Result()
	require.NoError(err)
	require.Len(newPolicyValues, 2)
	assert.Equal("allkeys-lfu", newPolicyValues[1], "valid maxmemory-policy should be applied")

	keepaliveResult := rClient.ConfigGet(context.TODO(), "tcp-keepalive")
	require.NoError(keepaliveResult.Err())
	keepaliveValues, err := keepaliveResult.Result()
	require.NoError(err)
	require.Len(keepaliveValues, 2)
	assert.Equal("120", keepaliveValues[1], "valid tcp-keepalive should be applied")

	// Cleanup: Delete the RedisFailover
	err = c.rfClient.DatabasesV1().RedisFailovers(currentNamespace).Delete(context.Background(), rfName, metav1.DeleteOptions{})
	require.NoError(err)
}
