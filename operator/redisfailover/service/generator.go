package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/operator/redisfailover/util"
)

const (
	redisConfigurationVolumeName = "redis-config"
	// Template used to build the Redis configuration.
	//
	// When TLS is enabled the plain port is disabled (port 0) and the
	// instance listens on tls-port using the same numeric value as
	// Spec.Redis.Port — TLS replaces plaintext rather than running
	// alongside it.
	redisConfigTemplate = `slaveof 127.0.0.1 {{.Spec.Redis.Port}}
{{- if tlsEnabled . }}
port 0
tls-port {{.Spec.Redis.Port}}
tls-cert-file ` + tlsCertFile + `
tls-key-file ` + tlsKeyFile + `
tls-ca-cert-file ` + tlsCAFile + `
tls-auth-clients {{ .Spec.TLS.AuthClients }}
tls-replication yes
{{- else }}
port {{.Spec.Redis.Port}}
{{- end }}
tcp-keepalive 60
save 900 1
save 300 10
user pinger -@all +ping on >pingpass
{{- range .Spec.Redis.CustomCommandRenames}}
rename-command "{{.From}}" "{{.To}}"
{{- end}}
`

	sentinelConfigTemplate = `
{{- if tlsEnabled . }}
port 0
tls-port 26379
tls-cert-file ` + tlsCertFile + `
tls-key-file ` + tlsKeyFile + `
tls-ca-cert-file ` + tlsCAFile + `
tls-auth-clients {{ .Spec.TLS.AuthClients }}
tls-replication yes
{{ end }}
{{- if .Spec.Sentinel.DisableMyMaster -}}
sentinel monitor {{.Name}} 127.0.0.1 {{.Spec.Redis.Port}} 2
sentinel down-after-milliseconds {{.Name}} 1000
sentinel failover-timeout {{.Name}} 3000
sentinel parallel-syncs {{.Name}} 2
{{- else -}}
sentinel monitor mymaster 127.0.0.1 {{.Spec.Redis.Port}} 2
sentinel down-after-milliseconds mymaster 1000
sentinel failover-timeout mymaster 3000
sentinel parallel-syncs mymaster 2
{{- end -}}
`

	redisShutdownConfigurationVolumeName   = "redis-shutdown-config"
	redisStartupConfigurationVolumeName    = "redis-startup-config"
	redisReadinessVolumeName               = "redis-readiness-config"
	redisStorageVolumeName                 = "redis-data"
	sentinelStartupConfigurationVolumeName = "sentinel-startup-config"

	graceTime = 30
)

// redisTemplateFuncs is the FuncMap shared by the redis/sentinel config
// templates. Only pure read-only helpers belong here.
var redisTemplateFuncs = template.FuncMap{
	"tlsEnabled": TLSEnabled,
}

// probeHost is the host the in-pod liveness and readiness probes dial.
//
// Plaintext keeps the "$(hostname)" form the operator has always emitted, so
// that upgrading the operator does not rewrite the pod template of a failover
// whose spec did not change. Under TLS the probes dial "localhost", which is
// a SAN entry on the generated certificate along with the loopback addresses;
// the bare pod hostname is not.
func probeHost(rf *redisfailoverv1.RedisFailover) string {
	if TLSEnabled(rf) {
		return "localhost"
	}
	return "$(hostname)"
}

// redisCLITLSFlags returns the trailing space-suffixed flag block that
// every redis-cli invocation in the pod scripts needs when TLS is on.
// Returns an empty string when TLS is disabled so the scripts stay
// untouched in the plaintext case.
func redisCLITLSFlags(rf *redisfailoverv1.RedisFailover) string {
	if !TLSEnabled(rf) {
		return ""
	}
	return fmt.Sprintf("--tls --cert %s --key %s --cacert %s ", tlsCertFile, tlsKeyFile, tlsCAFile)
}

// generateRedisCertificate builds the cert-manager Certificate that
// covers all Redis and Sentinel endpoints of the failover.
//
// SAN coverage:
//   - the four Service DNS names (redis headless, redis master,
//     redis slave, sentinel) in short, namespaced and FQDN form
//   - the per-pod DNS records of the redis headless service so the
//     operator can dial pods by stable name with TLS verification on
//   - localhost (DNS) and 127.0.0.1 / ::1 (IP) so the in-pod TLS
//     dials performed by liveness probes, the redis_exporter sidecar
//     and the sentinel monitor target verify cleanly
//
// Pod IPs are still not added: they are unstable, and the operator
// uses ServerName override in tls.Config to validate pod-IP-targeted
// dials against the headless DNS SAN instead.
func generateRedisCertificate(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, clusterDomain string) *cmapi.Certificate {
	name := GetTLSCertificateName(rf)
	secretName := GetTLSSecretName(rf)
	cm := rf.Spec.TLS.CertManager

	dnsNames, ipAddresses := redisCertificateSANs(rf, clusterDomain)
	for _, san := range cm.ExtraSANs {
		if ip := net.ParseIP(san); ip != nil {
			ipAddresses = append(ipAddresses, ip.String())
			continue
		}
		dnsNames = append(dnsNames, san)
	}

	cert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       rf.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: cmapi.CertificateSpec{
			SecretName:  secretName,
			IssuerRef:   cm.IssuerRef,
			DNSNames:    dnsNames,
			IPAddresses: ipAddresses,
			Usages: []cmapi.KeyUsage{
				cmapi.UsageDigitalSignature,
				cmapi.UsageKeyEncipherment,
				cmapi.UsageServerAuth,
				cmapi.UsageClientAuth,
			},
		},
	}
	if cm.Duration != nil {
		cert.Spec.Duration = cm.Duration
	}
	if cm.RenewBefore != nil {
		cert.Spec.RenewBefore = cm.RenewBefore
	}
	if cm.PrivateKey != nil {
		cert.Spec.PrivateKey = cm.PrivateKey
	}
	return cert
}

// generateRedisCACertSecret builds the Opaque Secret that holds only the CA
// certificate (ca.crt). It deliberately carries no tls.key, so RBAC can be
// scoped to this Secret alone, letting clients verify the Redis server without
// ever gaining access to a private key. caPEM is the ca.crt bytes read from the
// cluster's TLS secret.
func generateRedisCACertSecret(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, caPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            GetTLSCACertSecretName(rf),
			Namespace:       rf.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			tlsSecretCAKey: caPEM,
		},
	}
}

// tlsSecretContentHash returns a content hash of the TLS material the pods
// mount and of the tls-auth-clients directive they run with, or the empty
// string when there is no certificate to pin yet.
//
// Both tls.crt and ca.crt are covered. Redis loads the certificate and the CA
// bundle at startup and re-reads neither, so a change to either one only
// reaches the server by restarting the pod. tls.key is left out: cert-manager
// rotates it together with tls.crt, and there is no reason to derive a
// published annotation value from private key material.
//
// tls-auth-clients is covered for the same reason. It is a mutable spec field
// whose only carrier is the generated config file, which Redis also reads once
// at startup, so without it in the hash a change is admitted and then lands
// pod by pod at whatever unrelated restart comes next.
func tlsSecretContentHash(rf *redisfailoverv1.RedisFailover, secret *corev1.Secret) string {
	if secret == nil || len(secret.Data[tlsSecretKey]) == 0 {
		return ""
	}
	h := sha256.New()
	for _, key := range []string{tlsSecretKey, tlsSecretCAKey} {
		value := secret.Data[key]
		// Length-prefixed so that two different splits of the same
		// concatenated bytes cannot hash alike.
		h.Write([]byte(key + ":" + strconv.Itoa(len(value)) + ":"))
		h.Write(value)
	}
	authClients := tlsAuthClients(rf)
	h.Write([]byte("authClients:" + strconv.Itoa(len(authClients)) + ":"))
	h.Write([]byte(authClients))
	return hex.EncodeToString(h.Sum(nil))
}

// podAnnotations returns the pod template annotations with the TLS content
// hash stamped on top. The caller's map is left untouched, since it is the
// live spec map from the RedisFailover.
func podAnnotations(specAnnotations map[string]string, tlsHash string) map[string]string {
	if tlsHash == "" {
		return specAnnotations
	}
	return util.MergeAnnotations(specAnnotations, map[string]string{tlsSecretHashAnnotation: tlsHash})
}

// redisCertificateSANs returns the DNS and IP SANs the Certificate
// must cover. Each Service appears in three forms (short, namespaced,
// FQDN) so the same certificate validates regardless of how callers
// resolve it. localhost and the loopback IPs are included so the
// in-pod self-dials (liveness probes via -h localhost, the
// redis_exporter sidecar via 127.0.0.1, the sentinel monitor target
// 127.0.0.1) verify cleanly against the same cert.
//
// clusterDomain is the cluster's DNS suffix (e.g. "cluster.local",
// "cozy.local"). An empty value falls back to "cluster.local" so
// existing deployments that never set --cluster-domain keep working.
func redisCertificateSANs(rf *redisfailoverv1.RedisFailover, clusterDomain string) (dnsNames, ipAddresses []string) {
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	ns := rf.Namespace

	for _, svc := range []string{
		GetRedisName(rf),
		GetRedisMasterName(rf),
		GetRedisSlaveName(rf),
		GetSentinelName(rf),
	} {
		dnsNames = append(dnsNames,
			svc,
			fmt.Sprintf("%s.%s", svc, ns),
			fmt.Sprintf("%s.%s.svc", svc, ns),
			fmt.Sprintf("%s.%s.svc.%s", svc, ns, clusterDomain),
		)
	}

	// Headless service has per-pod DNS records:
	// <pod>.<headless>.<ns>.svc.<cluster-domain>. We don't know pod
	// count up front; cover the wildcard so any replica resolves
	// correctly.
	headless := GetRedisName(rf)
	dnsNames = append(dnsNames,
		fmt.Sprintf("*.%s", headless),
		fmt.Sprintf("*.%s.%s", headless, ns),
		fmt.Sprintf("*.%s.%s.svc", headless, ns),
		fmt.Sprintf("*.%s.%s.svc.%s", headless, ns, clusterDomain),
		"localhost",
	)

	ipAddresses = []string{"127.0.0.1", "::1"}
	return dnsNames, ipAddresses
}

func generateSentinelService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	sentinelTargetPort := intstr.FromInt(26379)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Sentinel.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       26379,
					TargetPort: sentinelTargetPort,
					Protocol:   "TCP",
				},
			},
		},
	}
}

func generateRedisService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	defaultAnnotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "http",
		"prometheus.io/path":   "/metrics",
	}
	annotations := util.MergeLabels(defaultAnnotations, rf.Spec.Redis.ServiceAnnotations)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{
					Port:     exporterPort,
					Protocol: corev1.ProtocolTCP,
					Name:     exporterPortName,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateRedisMasterService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisMasterName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	selectorLabels = util.MergeLabels(selectorLabels, map[string]string{
		redisRoleLabelKey: redisRoleLabelMaster,
	})
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Redis.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       rf.Spec.Redis.Port,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateRedisSlaveService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisSlaveName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	selectorLabels = util.MergeLabels(selectorLabels, map[string]string{
		redisRoleLabelKey: redisRoleLabelSlave,
	})
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Redis.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       rf.Spec.Redis.Port,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateSentinelConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(sentinelRoleName, rf.Name))

	tmpl, err := template.New("sentinel").Funcs(redisTemplateFuncs).Parse(sentinelConfigTemplate)
	if err != nil {
		panic(err)
	}

	var tplOutput bytes.Buffer
	if err := tmpl.Execute(&tplOutput, rf); err != nil {
		panic(err)
	}

	sentinelConfigFileContent := tplOutput.String()

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			sentinelConfigFileName: sentinelConfigFileContent,
		},
	}
}

func generateRedisConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, password string) *corev1.ConfigMap {
	name := GetRedisName(rf)
	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))

	tmpl, err := template.New("redis").Funcs(redisTemplateFuncs).Parse(redisConfigTemplate)
	if err != nil {
		panic(err)
	}

	var tplOutput bytes.Buffer
	if err := tmpl.Execute(&tplOutput, rf); err != nil {
		panic(err)
	}

	redisConfigFileContent := tplOutput.String()

	if password != "" {
		redisConfigFileContent = fmt.Sprintf("%s\nmasterauth %s\nrequirepass %s", redisConfigFileContent, password, password)
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       rf.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			redisConfigFileName: redisConfigFileContent,
		},
	}
}

func generateRedisShutdownConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisShutdownConfigMapName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace
	rfName := strings.ReplaceAll(strings.ToUpper(rf.Name), "-", "_")
	tlsFlags := redisCLITLSFlags(rf)

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	eng := EngineFor(rf)
	cli := eng.CLIBinary()
	authEnv := eng.CLIAuthEnvName()
	shutdownContent := fmt.Sprintf(`master=$(%[4]s %[6]s-h ${RFS_%[1]v_SERVICE_HOST} -p ${RFS_%[1]v_SERVICE_PORT_SENTINEL} --csv SENTINEL get-master-addr-by-name %[3]v | tr ',' ' ' | tr -d '\"' |cut -d' ' -f1)
if [ "$master" = "$(hostname -i)" ]; then
%[4]s %[6]s-h ${RFS_%[1]v_SERVICE_HOST} -p ${RFS_%[1]v_SERVICE_PORT_SENTINEL} SENTINEL failover %[3]v
sleep 31
fi
cmd="%[4]s %[6]s-p %[2]v"
if [ ! -z "${REDIS_PASSWORD}" ]; then
	export %[5]s=${REDIS_PASSWORD}
fi
save_command="${cmd} save"
eval $save_command`, rfName, port, rf.MasterName(), cli, authEnv, tlsFlags)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			"shutdown.sh": shutdownContent,
		},
	}
}

func generateRedisReadinessConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisReadinessName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace
	tlsFlags := redisCLITLSFlags(rf)

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	eng := EngineFor(rf)
	cli := eng.CLIBinary()
	authEnv := eng.CLIAuthEnvName()
	readinessContent := fmt.Sprintf(`ROLE="role"
ROLE_MASTER="role:master"
ROLE_SLAVE="role:slave"
IN_SYNC="master_sync_in_progress:1"
NO_MASTER="master_host:127.0.0.1"

cmd="%[2]s %[4]s-p %[1]v"
if [ ! -z "${REDIS_PASSWORD}" ]; then
	export %[3]s=${REDIS_PASSWORD}
fi

cmd="${cmd} info replication"

check_master(){
		exit 0
}

check_slave(){
		in_sync=$(echo "${cmd} | grep ${IN_SYNC} | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
		no_master=$(echo "${cmd} | grep ${NO_MASTER} | tr -d \"\\r\" | tr -d \"\\n\"" |  xargs -0 sh -c)

		if [ -z "$in_sync" ] && [ -z "$no_master" ]; then
				exit 0
		fi

		exit 1
}

role=$(echo "${cmd} | grep $ROLE | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
case $role in
		$ROLE_MASTER)
				check_master
				;;
		$ROLE_SLAVE)
				check_slave
				;;
		*)
				echo "unexpected"
				exit 1
esac`, port, cli, authEnv, tlsFlags)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			"ready.sh": readinessContent,
		},
	}
}

func generateRedisStatefulSet(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) *appsv1.StatefulSet {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	eng := EngineFor(rf)
	redisCommand := getRedisCommand(rf, eng)
	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	labels = util.MergeLabels(labels, generateRedisDefaultRoleLabel())

	volumeMounts := getRedisVolumeMounts(rf)
	volumes := getRedisVolumes(rf)
	terminationGracePeriodSeconds := getTerminationGracePeriodSeconds(rf)

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Annotations:     rf.Annotations,
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &rf.Spec.Redis.Replicas,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations(rf.Spec.Redis.PodAnnotations, tlsHash),
				},
				Spec: corev1.PodSpec{
					Affinity:                      getAffinity(rf.Spec.Redis.Affinity, labels),
					Tolerations:                   rf.Spec.Redis.Tolerations,
					TopologySpreadConstraints:     rf.Spec.Redis.TopologySpreadConstraints,
					NodeSelector:                  rf.Spec.Redis.NodeSelector,
					SecurityContext:               getSecurityContext(rf.Spec.Redis.SecurityContext),
					HostNetwork:                   rf.Spec.Redis.HostNetwork,
					DNSPolicy:                     getDnsPolicy(rf.Spec.Redis.DNSPolicy),
					ImagePullSecrets:              rf.Spec.Redis.ImagePullSecrets,
					PriorityClassName:             rf.Spec.Redis.PriorityClassName,
					ServiceAccountName:            rf.Spec.Redis.ServiceAccountName,
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					Containers: []corev1.Container{
						{
							Name:            "redis",
							Image:           rf.Spec.Redis.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
							Ports: []corev1.ContainerPort{
								{
									Name:          "redis",
									ContainerPort: rf.Spec.Redis.Port,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: volumeMounts,
							Command:      redisCommand,
							Resources:    rf.Spec.Redis.Resources,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "/redis-shutdown/shutdown.sh"},
									},
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if rf.Spec.Redis.Storage.PersistentVolumeClaim != nil {
		pvc := corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              rf.Spec.Redis.Storage.PersistentVolumeClaim.Name,
				Labels:            rf.Spec.Redis.Storage.PersistentVolumeClaim.Labels,
				Annotations:       rf.Spec.Redis.Storage.PersistentVolumeClaim.Annotations,
				CreationTimestamp: metav1.Time{},
			},
			Spec:   rf.Spec.Redis.Storage.PersistentVolumeClaim.Spec,
			Status: rf.Spec.Redis.Storage.PersistentVolumeClaim.Status,
		}
		if !rf.Spec.Redis.Storage.KeepAfterDeletion {
			// Set an owner reference so the persistent volumes are deleted when the RF is
			pvc.OwnerReferences = ownerRefs
		}
		ss.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			pvc,
		}
	}

	if rf.Spec.Redis.CustomLivenessProbe != nil {
		ss.Spec.Template.Spec.Containers[0].LivenessProbe = rf.Spec.Redis.CustomLivenessProbe
	} else {
		ss.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf("%s %s-h %s -p %v --user pinger --pass pingpass --no-auth-warning ping | grep PONG", eng.CLIBinary(), redisCLITLSFlags(rf), probeHost(rf), rf.Spec.Redis.Port),
					},
				},
			},
		}
	}

	if rf.Spec.Redis.CustomReadinessProbe != nil {
		ss.Spec.Template.Spec.Containers[0].ReadinessProbe = rf.Spec.Redis.CustomReadinessProbe
	} else {
		ss.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/redis-readiness/ready.sh"},
				},
			},
		}
	}

	if rf.Spec.Redis.CustomStartupProbe != nil {
		ss.Spec.Template.Spec.Containers[0].StartupProbe = rf.Spec.Redis.CustomStartupProbe
	} else if rf.Spec.Redis.StartupConfigMap != "" {
		ss.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/redis-startup/startup.sh"},
				},
			},
		}
	}

	if rf.Spec.Redis.Exporter.Enabled {
		exporter := createRedisExporterContainer(rf)
		ss.Spec.Template.Spec.Containers = append(ss.Spec.Template.Spec.Containers, exporter)
	}

	if rf.Spec.Redis.InitContainers != nil {
		initContainers := getInitContainersWithRedisEnv(rf)
		ss.Spec.Template.Spec.InitContainers = append(ss.Spec.Template.Spec.InitContainers, initContainers...)
	}

	if rf.Spec.Redis.ExtraContainers != nil {
		extraContainers := getExtraContainersWithRedisEnv(rf)
		ss.Spec.Template.Spec.Containers = append(ss.Spec.Template.Spec.Containers, extraContainers...)
	}

	redisEnv := getRedisEnv(rf)
	ss.Spec.Template.Spec.Containers[0].Env = append(ss.Spec.Template.Spec.Containers[0].Env, redisEnv...)

	return ss
}

func generateSentinelDeployment(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) *appsv1.Deployment {
	name := GetSentinelName(rf)
	configMapName := GetSentinelName(rf)
	namespace := rf.Namespace

	eng := EngineFor(rf)
	sentinelCommand := getSentinelCommand(rf, eng)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	volumeMounts := getSentinelVolumeMounts(rf)
	volumes := getSentinelVolumes(rf, configMapName)

	sd := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rf.Spec.Sentinel.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations(rf.Spec.Sentinel.PodAnnotations, tlsHash),
				},
				Spec: corev1.PodSpec{
					Affinity:                  getAffinity(rf.Spec.Sentinel.Affinity, labels),
					Tolerations:               rf.Spec.Sentinel.Tolerations,
					TopologySpreadConstraints: rf.Spec.Sentinel.TopologySpreadConstraints,
					NodeSelector:              rf.Spec.Sentinel.NodeSelector,
					SecurityContext:           getSecurityContext(rf.Spec.Sentinel.SecurityContext),
					HostNetwork:               rf.Spec.Sentinel.HostNetwork,
					DNSPolicy:                 getDnsPolicy(rf.Spec.Sentinel.DNSPolicy),
					ImagePullSecrets:          rf.Spec.Sentinel.ImagePullSecrets,
					PriorityClassName:         rf.Spec.Sentinel.PriorityClassName,
					ServiceAccountName:        rf.Spec.Sentinel.ServiceAccountName,
					InitContainers: []corev1.Container{
						{
							Name:            "sentinel-config-copy",
							Image:           rf.Spec.Sentinel.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ConfigCopy.ContainerSecurityContext),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "sentinel-config",
									MountPath: "/redis",
								},
								{
									Name:      "sentinel-config-writable",
									MountPath: "/redis-writable",
								},
							},
							Command: []string{
								"cp",
								fmt.Sprintf("/redis/%s", sentinelConfigFileName),
								fmt.Sprintf("/redis-writable/%s", sentinelConfigFileName),
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "sentinel",
							Image:           rf.Spec.Sentinel.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ContainerSecurityContext),
							Ports: []corev1.ContainerPort{
								{
									Name:          "sentinel",
									ContainerPort: 26379,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: volumeMounts,
							Command:      sentinelCommand,
							Resources:    rf.Spec.Sentinel.Resources,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if rf.Spec.Sentinel.CustomLivenessProbe != nil {
		sd.Spec.Template.Spec.Containers[0].LivenessProbe = rf.Spec.Sentinel.CustomLivenessProbe
	} else {
		sd.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf("%s %s-h %s -p 26379 ping", eng.CLIBinary(), redisCLITLSFlags(rf), probeHost(rf)),
					},
				},
			},
		}
	}

	if rf.Spec.Sentinel.CustomReadinessProbe != nil {
		sd.Spec.Template.Spec.Containers[0].ReadinessProbe = rf.Spec.Sentinel.CustomReadinessProbe
	} else {
		probeCommand := fmt.Sprintf("%s %s-h %s -p 26379 sentinel get-master-addr-by-name %s | head -n 1 | grep -vq '127.0.0.1'", eng.CLIBinary(), redisCLITLSFlags(rf), probeHost(rf), rf.MasterName())
		sd.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh",
						"-c",
						probeCommand,
					},
				},
			},
		}
	}

	if rf.Spec.Sentinel.CustomStartupProbe != nil {
		sd.Spec.Template.Spec.Containers[0].StartupProbe = rf.Spec.Sentinel.CustomStartupProbe
	} else if rf.Spec.Sentinel.StartupConfigMap != "" {
		sd.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/sentinel-startup/startup.sh"},
				},
			},
		}
	}

	if rf.Spec.Sentinel.Exporter.Enabled {
		exporter := createSentinelExporterContainer(rf)
		sd.Spec.Template.Spec.Containers = append(sd.Spec.Template.Spec.Containers, exporter)
	}
	if rf.Spec.Sentinel.InitContainers != nil {
		sd.Spec.Template.Spec.InitContainers = append(sd.Spec.Template.Spec.InitContainers, rf.Spec.Sentinel.InitContainers...)
	}

	if rf.Spec.Sentinel.ExtraContainers != nil {
		sd.Spec.Template.Spec.Containers = append(sd.Spec.Template.Spec.Containers, rf.Spec.Sentinel.ExtraContainers...)
	}

	return sd
}

func generatePodDisruptionBudget(name string, namespace string, labels map[string]string, ownerRefs []metav1.OwnerReference, minAvailable intstr.IntOrString) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
		},
	}
}

var exporterDefaultResourceRequirements = corev1.ResourceRequirements{
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultLimitCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultLimitMemory),
	},
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultRequestCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultRequestMemory),
	},
}

func createRedisExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Redis.Exporter.Resources != nil {
		resources = *rf.Spec.Redis.Exporter.Resources
	}
	container := corev1.Container{
		Name:            exporterContainerName,
		Image:           rf.Spec.Redis.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Redis.Exporter.Args,
		Env: append(rf.Spec.Redis.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: exporterPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	redisEnv := getRedisEnv(rf)
	container.Env = append(container.Env, redisEnv...)

	if TLSEnabled(rf) {
		container.VolumeMounts = append(container.VolumeMounts, tlsVolumeMount())
		container.Env = append(container.Env, redisExporterTLSEnv()...)
	}

	return container
}

func createSentinelExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Sentinel.Exporter.Resources != nil {
		resources = *rf.Spec.Sentinel.Exporter.Resources
	}

	addrScheme := "redis"
	if TLSEnabled(rf) {
		addrScheme = "rediss"
	}

	container := corev1.Container{
		Name:            sentinelExporterContainerName,
		Image:           rf.Spec.Sentinel.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Sentinel.Exporter.Args,
		Env: append(rf.Spec.Sentinel.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}, corev1.EnvVar{
			Name:  "REDIS_EXPORTER_WEB_LISTEN_ADDRESS",
			Value: fmt.Sprintf("0.0.0.0:%[1]v", sentinelExporterPort),
		}, corev1.EnvVar{
			Name:  "REDIS_ADDR",
			Value: fmt.Sprintf("%s://127.0.0.1:26379", addrScheme),
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: sentinelExporterPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	if TLSEnabled(rf) {
		container.VolumeMounts = append(container.VolumeMounts, tlsVolumeMount())
		container.Env = append(container.Env, redisExporterTLSEnv()...)
	}

	return container
}

// redisExporterTLSEnv returns the env vars that point oliver006/redis_exporter
// at the mounted certificate. The exporter uses the certificate itself as
// the client certificate (the same Secret holds the cluster cert/key/CA),
// which is sufficient for read-only INFO/CONFIG GET queries.
func redisExporterTLSEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "REDIS_EXPORTER_TLS_CLIENT_CERT_FILE", Value: tlsCertFile},
		{Name: "REDIS_EXPORTER_TLS_CLIENT_KEY_FILE", Value: tlsKeyFile},
		{Name: "REDIS_EXPORTER_TLS_CA_CERT_FILE", Value: tlsCAFile},
	}
}

func getAffinity(affinity *corev1.Affinity, labels map[string]string) *corev1.Affinity {
	if affinity != nil {
		return affinity
	}

	// Return a SOFT anti-affinity
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: hostnameTopologyKey,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: labels,
						},
					},
				},
			},
		},
	}
}

func getSecurityContext(secctx *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	if secctx != nil {
		return secctx
	}

	defaultUserAndGroup := int64(1000)
	runAsNonRoot := true

	return &corev1.PodSecurityContext{
		RunAsUser:    &defaultUserAndGroup,
		RunAsGroup:   &defaultUserAndGroup,
		RunAsNonRoot: &runAsNonRoot,
		FSGroup:      &defaultUserAndGroup,
	}
}

func getContainerSecurityContext(secctx *corev1.SecurityContext) *corev1.SecurityContext {
	if secctx != nil {
		return secctx
	}

	capabilities := &corev1.Capabilities{
		Add: []corev1.Capability{},
		Drop: []corev1.Capability{
			"ALL",
		},
	}
	privileged := false
	defaultUserAndGroup := int64(1000)
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true

	return &corev1.SecurityContext{
		Capabilities:             capabilities,
		Privileged:               &privileged,
		RunAsUser:                &defaultUserAndGroup,
		RunAsGroup:               &defaultUserAndGroup,
		RunAsNonRoot:             &runAsNonRoot,
		ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
	}
}

func getDnsPolicy(dnspolicy corev1.DNSPolicy) corev1.DNSPolicy {
	if dnspolicy == "" {
		return corev1.DNSClusterFirst
	}
	return dnspolicy
}

func getQuorum(rf *redisfailoverv1.RedisFailover) int32 {
	return rf.Spec.Sentinel.Replicas/2 + 1
}

func getRedisVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      redisConfigurationVolumeName,
			MountPath: "/redis",
		},
		{
			Name:      redisShutdownConfigurationVolumeName,
			MountPath: "/redis-shutdown",
		},
		{
			Name:      redisReadinessVolumeName,
			MountPath: "/redis-readiness",
		},
		{
			Name:      getRedisDataVolumeName(rf),
			MountPath: "/data",
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      redisStartupConfigurationVolumeName,
			MountPath: "/redis-startup",
		}

		volumeMounts = append(volumeMounts, startupVolumeMount)
	}

	if TLSEnabled(rf) {
		volumeMounts = append(volumeMounts, tlsVolumeMount())
	}

	if rf.Spec.Redis.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Redis.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func getSentinelVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "sentinel-config-writable",
			MountPath: "/redis",
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      "sentinel-startup-config",
			MountPath: "/sentinel-startup",
		}
		volumeMounts = append(volumeMounts, startupVolumeMount)
	}
	if TLSEnabled(rf) {
		volumeMounts = append(volumeMounts, tlsVolumeMount())
	}
	if rf.Spec.Sentinel.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Sentinel.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func tlsVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      tlsVolumeName,
		MountPath: tlsMountPath,
		ReadOnly:  true,
	}
}

// tlsVolume builds the Secret volume carrying the certificate, its private
// key and the CA bundle.
//
// The kubelet defaults secret files to 0644, which leaves the private key
// readable by every uid in the pod — sidecars included, and those can be
// given a securityContext of their own. Tightening the mode to 0440 relies
// on the files being group-owned by a gid the containers run with, which is
// what an fsGroup on the pod does; without one the kubelet leaves them owned
// by root:root and 0440 would hide the key from any container running as
// non-root. So the mode is only tightened when the pod declares an fsGroup,
// and a caller-supplied securityContext without one keeps the default.
func tlsVolume(secretName string, podSecurityContext *corev1.PodSecurityContext) corev1.Volume {
	volume := corev1.Volume{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
			},
		},
	}
	if podSecurityContext != nil && podSecurityContext.FSGroup != nil {
		mode := tlsVolumeMode
		volume.VolumeSource.Secret.DefaultMode = &mode
	}
	return volume
}

func getRedisVolumes(rf *redisfailoverv1.RedisFailover) []corev1.Volume {
	configMapName := GetRedisName(rf)
	shutdownConfigMapName := GetRedisShutdownConfigMapName(rf)
	readinessConfigMapName := GetRedisReadinessName(rf)

	executeMode := int32(0744)
	volumes := []corev1.Volume{
		{
			Name: redisConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: redisShutdownConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: shutdownConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
		{
			Name: redisReadinessVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: readinessConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Redis.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: redisStartupConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: startupVolumeName,
					},
					DefaultMode: &executeMode,
				},
			},
		}
		volumes = append(volumes, startupVolume)
	}

	if TLSEnabled(rf) {
		volumes = append(volumes, tlsVolume(GetTLSSecretName(rf), getSecurityContext(rf.Spec.Redis.SecurityContext)))
	}

	if rf.Spec.Redis.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Redis.ExtraVolumes...)
	}

	dataVolume := getRedisDataVolume(rf)
	if dataVolume != nil {
		volumes = append(volumes, *dataVolume)
	}

	return volumes
}

func getSentinelVolumes(rf *redisfailoverv1.RedisFailover, configMapName string) []corev1.Volume {
	executeMode := int32(0744)

	volumes := []corev1.Volume{
		{
			Name: "sentinel-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: "sentinel-config-writable",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Sentinel.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: sentinelStartupConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: startupVolumeName,
					},
					DefaultMode: &executeMode,
				},
			},
		}
		volumes = append(volumes, startupVolume)
	}

	if TLSEnabled(rf) {
		volumes = append(volumes, tlsVolume(GetTLSSecretName(rf), getSecurityContext(rf.Spec.Sentinel.SecurityContext)))
	}

	if rf.Spec.Sentinel.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Sentinel.ExtraVolumes...)
	}

	return volumes
}

func getRedisDataVolume(rf *redisfailoverv1.RedisFailover) *corev1.Volume {
	// This will find the volumed desired by the user. If no volume defined
	// an EmptyDir will be used by default
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return nil
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: rf.Spec.Redis.Storage.EmptyDir,
			},
		}
	default:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}
	}
}

func getRedisDataVolumeName(rf *redisfailoverv1.RedisFailover) string {
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return rf.Spec.Redis.Storage.PersistentVolumeClaim.Name
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return redisStorageVolumeName
	default:
		return redisStorageVolumeName
	}
}

func getRedisCommand(rf *redisfailoverv1.RedisFailover, eng DatabaseEngineProvider) []string {
	if len(rf.Spec.Redis.Command) > 0 {
		return rf.Spec.Redis.Command
	}
	return []string{
		eng.ServerBinary(),
		fmt.Sprintf("/redis/%s", redisConfigFileName),
	}
}

func getSentinelCommand(rf *redisfailoverv1.RedisFailover, eng DatabaseEngineProvider) []string {
	if len(rf.Spec.Sentinel.Command) > 0 {
		return rf.Spec.Sentinel.Command
	}
	return []string{
		eng.ServerBinary(),
		fmt.Sprintf("/redis/%s", sentinelConfigFileName),
		"--sentinel",
	}
}

func pullPolicy(specPolicy corev1.PullPolicy) corev1.PullPolicy {
	if specPolicy == "" {
		return corev1.PullAlways
	}
	return specPolicy
}

func getTerminationGracePeriodSeconds(rf *redisfailoverv1.RedisFailover) int64 {
	if rf.Spec.Redis.TerminationGracePeriodSeconds > 0 {
		return rf.Spec.Redis.TerminationGracePeriodSeconds
	}
	return 30
}

func getExtraContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	extraContainers := getContainersWithRedisEnv(rf.Spec.Redis.ExtraContainers, env)

	return extraContainers
}

func getInitContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	initContainers := getContainersWithRedisEnv(rf.Spec.Redis.InitContainers, env)

	return initContainers
}

func getContainersWithRedisEnv(cs []corev1.Container, e []corev1.EnvVar) []corev1.Container {
	var containers []corev1.Container
	for _, c := range cs {
		c.Env = append(c.Env, e...)
		containers = append(containers, c)
	}

	return containers
}

func getRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	var env []corev1.EnvVar

	scheme := "redis"
	if TLSEnabled(rf) {
		scheme = "rediss"
	}
	env = append(env, corev1.EnvVar{
		Name:  "REDIS_ADDR",
		Value: fmt.Sprintf("%s://127.0.0.1:%[2]v", scheme, rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_PORT",
		Value: fmt.Sprintf("%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_USER",
		Value: "default",
	})

	if rf.Spec.Auth.SecretPath != "" {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rf.Spec.Auth.SecretPath,
					},
					Key: "password",
				},
			},
		})
	}

	return env
}
