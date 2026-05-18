package redisfailover

import (
	"context"
	"regexp"
	"time"

	"github.com/spotahome/kooper/v2/controller"
	"github.com/spotahome/kooper/v2/controller/leaderelection"
	kooperlog "github.com/spotahome/kooper/v2/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	rfservice "github.com/freshworks/redis-operator/operator/redisfailover/service"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
)

const (
	resync             = 30 * time.Second
	operatorName       = "redis-operator"
	lockKeyPrefix      = "redis-failover-lease"
	rfOperatorGroupKey = "redis-failover.freshworks.com/operator-group"
)

// New will create an operator that is responsible of managing all the required stuff
// to create redis failovers.
func New(cfg Config, k8sService k8s.Services, k8sClient kubernetes.Interface, lockNamespace string, redisClient redis.Client, kooperMetricsRecorder metrics.Recorder, logger log.Logger) (controller.Controller, error) {
	// Create internal services.
	rfService := rfservice.NewRedisFailoverKubeClient(k8sService, logger, kooperMetricsRecorder)
	rfChecker := rfservice.NewRedisFailoverChecker(k8sService, redisClient, logger, kooperMetricsRecorder)
	rfHealer := rfservice.NewRedisFailoverHealer(k8sService, redisClient, logger)

	// Create the handlers.
	rfHandler := NewRedisFailoverHandler(cfg, rfService, rfChecker, rfHealer, k8sService, kooperMetricsRecorder, logger)
	rfRetriever := NewRedisFailoverRetriever(cfg, k8sService)

	kooperLogger := kooperlogger{Logger: logger.WithField("operator", "redisfailover")}
	// Leader election service: one lease per operator group so each deployment can be leader for its group.
	lockKey := lockKeyPrefix + "-" + cfg.OperatorGroupID
	leSVC, err := leaderelection.NewDefault(lockKey, lockNamespace, k8sClient, kooperLogger)
	if err != nil {
		return nil, err
	}

	// Create our controller.
	return controller.New(&controller.Config{
		Handler:           rfHandler,
		Retriever:         rfRetriever,
		LeaderElector:     leSVC,
		MetricsRecorder:   kooperMetricsRecorder,
		Logger:            kooperLogger,
		Name:              "redisfailover",
		ResyncInterval:    resync,
		ConcurrentWorkers: cfg.Concurrency,
	})
}

func NewRedisFailoverRetriever(cfg Config, cli k8s.Services) controller.Retriever {
	isNamespaceSupported := func(rf redisfailoverv1.RedisFailover) bool {
		match, _ := regexp.Match(cfg.SupportedNamespacesRegex, []byte(rf.Namespace))
		return match
	}

	// Server-side label selector so only RF CRs for this group are listed/watched.
	groupSelector := labels.SelectorFromSet(map[string]string{
		rfOperatorGroupKey: cfg.OperatorGroupID,
	}).String()

	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return listRedisFailoversWithNamespaceFilter(context.Background(), cli, groupSelector, options, isNamespaceSupported)
		},
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return listRedisFailoversWithNamespaceFilter(ctx, cli, groupSelector, options, isNamespaceSupported)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return watchRedisFailoversWithNamespaceFilter(context.Background(), cli, groupSelector, options, isNamespaceSupported)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return watchRedisFailoversWithNamespaceFilter(ctx, cli, groupSelector, options, isNamespaceSupported)
		},
	}
	return controller.MustRetrieverFromListerWatcher(&redisFailoverListerWatcher{ListWatch: lw})
}

// redisFailoverListerWatcher wraps ListWatch and opts out of client-go WatchList semantics
// (SendInitialEvents), which custom CRD watches and older apiservers may not support.
// Recognized by client-go v0.35+ reflectors via IsWatchListSemanticsUnSupported.
type redisFailoverListerWatcher struct {
	*cache.ListWatch
}

func (redisFailoverListerWatcher) IsWatchListSemanticsUnSupported() bool {
	return true
}

func listRedisFailoversWithNamespaceFilter(
	ctx context.Context,
	cli k8s.Services,
	groupSelector string,
	options metav1.ListOptions,
	isNamespaceSupported func(redisfailoverv1.RedisFailover) bool,
) (runtime.Object, error) {
	options.LabelSelector = groupSelector
	rfList, err := cli.ListRedisFailovers(ctx, "", options)
	if err != nil {
		return rfList, err
	}

	targetRFList := make([]redisfailoverv1.RedisFailover, 0)
	for _, rf := range rfList.Items {
		if isNamespaceSupported(rf) {
			targetRFList = append(targetRFList, rf)
		}
	}
	rfList.Items = targetRFList

	return rfList, nil
}

func watchRedisFailoversWithNamespaceFilter(
	ctx context.Context,
	cli k8s.Services,
	groupSelector string,
	options metav1.ListOptions,
	isNamespaceSupported func(redisfailoverv1.RedisFailover) bool,
) (watch.Interface, error) {
	options.LabelSelector = groupSelector
	watcher, err := cli.WatchRedisFailovers(ctx, "", options)
	if err != nil {
		return nil, err
	}
	return watch.Filter(watcher, func(event watch.Event) (watch.Event, bool) {
		rf, ok := event.Object.(*redisfailoverv1.RedisFailover)
		if !ok {
			return event, false
		}
		return event, isNamespaceSupported(*rf)
	}), nil
}

type kooperlogger struct {
	log.Logger
}

func (k kooperlogger) WithKV(kv kooperlog.KV) kooperlog.Logger {
	return kooperlogger{Logger: k.WithFields(kv)}
}
