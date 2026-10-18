package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

const orphanFinalizer = "gong.io/orphan-tagger"

// providerConfigGVR is the Crossplane AWS ProviderConfig, which carries the
// assumeRoleChain used to reach resources living in another AWS account.
var providerConfigGVR = schema.GroupVersionResource{
	Group: "aws.upbound.io", Version: "v1beta1", Resource: "providerconfigs",
}

var (
	metricsAddr   = flag.String("metrics-addr", ":8080", "Address to expose Prometheus metrics")
	kubeContext   = flag.String("context", "", "Kubeconfig context to use (for local debug)")
	kubeconfig    = flag.String("kubeconfig", "", "Path to kubeconfig (defaults to in-cluster config)")
	debug         = flag.Bool("debug", false, "Enable verbose per-resource debug logging")
	pollInterval  = flag.Duration("poll-interval", 30*time.Second, "How often to scan all CRDs for terminating MRs")
	maxConcurrent = flag.Int("max-concurrent", 5, "Max resources tagged in parallel (caps AWS API rate)")
	retryAttempts = flag.Int("aws-retry-attempts", 10, "Max AWS SDK attempts per API call (throttling backoff)")

	mrTerminating = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orphan_tagger_mr_terminating",
		Help: "1 if the AWS MR has deletionTimestamp set and gong.io/orphan-tagger finalizer",
	}, []string{"name", "kind", "group", "deletion_timestamp", "finalizers"})

	mrFinalizerCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orphan_tagger_mr_finalizer_count",
		Help: "Number of finalizers on the AWS managed resource",
	}, []string{"name", "kind", "group"})

	taggedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orphan_tagger_tagged_total",
		Help: "Total AWS resources successfully tagged with crossplane-orphaned=true",
	}, []string{"kind", "group"})

	tagErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orphan_tagger_tag_error_total",
		Help: "Total errors tagging AWS resources",
	}, []string{"kind", "group"})

	watchedCRDs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orphan_tagger_watched_crds",
		Help: "Number of AWS CRDs scanned each poll cycle",
	})

	lastScanDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orphan_tagger_last_scan_duration_seconds",
		Help: "Duration of the last full scan cycle",
	})

	nonTaggableTerminating = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orphan_tagger_non_taggable_mr_terminating",
		Help: "1 if the AWS MR is terminating but has no tags field in CRD schema — finalizer removed without AWS tagging",
	}, []string{"name", "kind", "group", "deletion_timestamp"})

	assumeRoleErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orphan_tagger_assume_role_error_total",
		Help: "Total failures resolving a ProviderConfig's assumeRoleChain for cross-account tagging",
	}, []string{"provider_config"})

	throttledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orphan_tagger_throttled_total",
		Help: "Total tag attempts abandoned due to AWS throttling (retried next poll cycle)",
	}, []string{"kind", "group"})

	crdInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orphan_tagger_crd_info",
		Help: "1 for each AWS CRD watched; taggable=true if CRD schema has spec.forProvider.tags",
	}, []string{"kind", "group", "taggable"})
)

func init() {
	prometheus.MustRegister(mrTerminating, mrFinalizerCount, taggedTotal, tagErrorTotal, watchedCRDs, lastScanDuration, nonTaggableTerminating, crdInfo, assumeRoleErrorTotal, throttledTotal)
}

func main() {
	flag.Parse()

	cfg, err := buildKubeConfig()
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}

	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(func(format string, args ...interface{}) {
		log.Printf("[event] "+format, args...)
	})
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kc.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "orphan-tagger-watcher"})

	// Adaptive retry mode adds client-side rate limiting on top of backoff, which the
	// default 3-attempt standard mode lacks — a full scan of a large cluster otherwise
	// exhausts the Resource Groups Tagging API quota and fails with
	// "exceeded maximum number of attempts, 3 ... ThrottledException".
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRetryMaxAttempts(*retryAttempts),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
	)
	if err != nil {
		log.Fatalf("AWS config: %v", err)
	}

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Printf("metrics listening on %s", *metricsAddr)
		if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	// Track which resources have been queued to avoid duplicate tagging within a scan cycle.
	inFlight := sync.Map{}
	// seenKeys tracks name/kind/group seen in the last scan — used to clean up stale metrics.
	var prevSeen map[string]bool

	log.Printf("starting poll loop (interval=%s)", *pollInterval)
	for {
		start := time.Now()
		seen, err := scanAndTag(context.Background(), dc, awsCfg, &inFlight, recorder)
		if err != nil {
			log.Printf("scan error: %v", err)
		}
		// Clean up metrics for MRs that disappeared since last scan.
		// Skip cleanup if scan failed (seen == nil) to avoid wiping all metrics on transient errors.
		if seen != nil {
			for key := range prevSeen {
				if !seen[key] {
					parts := strings.SplitN(key, "/", 3)
					if len(parts) == 3 {
						mrTerminating.DeletePartialMatch(prometheus.Labels{"name": parts[0], "kind": parts[1], "group": parts[2]})
						mrFinalizerCount.DeleteLabelValues(parts[0], parts[1], parts[2])
						nonTaggableTerminating.DeletePartialMatch(prometheus.Labels{"name": parts[0], "kind": parts[1], "group": parts[2]})
					}
				}
			}
			prevSeen = seen
		}
		lastScanDuration.Set(time.Since(start).Seconds())
		time.Sleep(*pollInterval)
	}
}

func buildKubeConfig() (*rest.Config, error) {
	if *kubeconfig != "" || *kubeContext != "" {
		loadRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if *kubeconfig != "" {
			loadRules.ExplicitPath = *kubeconfig
		}
		overrides := &clientcmd.ConfigOverrides{}
		if *kubeContext != "" {
			overrides.CurrentContext = *kubeContext
		}
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadRules, overrides).ClientConfig()
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	}
	return cfg, nil
}

// scanAndTag does a single pass: discovers all AWS CRDs, lists their resources,
// finds any that are terminating with our finalizer, and tags+unblocks them.
// Returns the set of "name/kind/group" keys seen this scan for stale metric cleanup.
func scanAndTag(ctx context.Context, dc dynamic.Interface, awsCfg aws.Config, inFlight *sync.Map, recorder record.EventRecorder) (map[string]bool, error) {
	gvrs, taggableKinds, err := discoverAWSGroups(ctx, dc)
	if err != nil {
		return nil, fmt.Errorf("discovering groups: %w", err)
	}
	watchedCRDs.Set(float64(len(gvrs)))
	seen := make(map[string]bool)

	// Bound parallel AWS calls — an unbounded goroutine per terminating MR is what
	// exhausts the Resource Groups Tagging API quota on large clusters.
	sem := make(chan struct{}, *maxConcurrent)

	if *debug {
		log.Printf("[debug] scanning %d CRDs", len(gvrs))
	}

	for _, gvr := range gvrs {
		list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			if *debug {
				log.Printf("[debug] list %s: %v", gvr.Resource, err)
			}
			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			name := obj.GetName()
			kind := obj.GetKind()
			group := gvr.Group
			finalizers := obj.GetFinalizers()

			// Track this resource as seen this scan cycle.
			seen[name+"/"+kind+"/"+group] = true

			// Update metrics for every resource.
			mrFinalizerCount.WithLabelValues(name, kind, group).Set(float64(len(finalizers)))
			if obj.GetDeletionTimestamp() != nil {
				deletionTS := obj.GetDeletionTimestamp().UTC().Format("2006-01-02T15:04:05Z")
				finalizerList := strings.Join(finalizers, ",")
				mrTerminating.WithLabelValues(name, kind, group, deletionTS, finalizerList).Set(1)
			} else {
				// Clean up any previous terminating metric for this resource
				mrTerminating.DeletePartialMatch(prometheus.Labels{"name": name, "kind": kind, "group": group})
				continue
			}

			// Only act on terminating resources with our finalizer.
			if !slices.Contains(finalizers, orphanFinalizer) {
				continue
			}

			isTaggable := taggableKinds[kind]

			arn, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "arn")
			if arn == "" {
				// Fallback for AccessEntry which uses accessEntryArn instead of arn
				arn, _, _ = unstructured.NestedString(obj.Object, "status", "atProvider", "accessEntryArn")
			}
			// Non-taggable resources (e.g. RolePolicy, Alias) have no ARN — skip the
			// ARN check so the finalizer is still removed.
			if arn == "" && isTaggable {
				continue
			}

			// Track non-taggable terminating MRs as a separate metric for observability
			if !isTaggable {
				deletionTS := obj.GetDeletionTimestamp().UTC().Format("2006-01-02T15:04:05Z")
				nonTaggableTerminating.WithLabelValues(name, kind, group, deletionTS).Set(1)
			}

			// Use apiVersion+kind+name as key to avoid double-processing across scan cycles.
			key := fmt.Sprintf("%s/%s/%s", obj.GetAPIVersion(), kind, name)
			if _, loaded := inFlight.LoadOrStore(key, true); loaded {
				continue
			}

			sem <- struct{}{}
			go func(obj unstructured.Unstructured, gvr schema.GroupVersionResource, key string) {
				defer func() { <-sem }()
				defer inFlight.Delete(key)
				if err := tagAndRemoveFinalizer(ctx, dc, &obj, gvr, awsCfg, isTaggable, recorder); err != nil {
					log.Printf("error processing %s/%s: %v", obj.GetKind(), obj.GetName(), err)
				}
			}(*obj, gvr, key)
		}
	}
	return seen, nil
}

func tagAndRemoveFinalizer(ctx context.Context, dc dynamic.Interface, obj *unstructured.Unstructured, gvr schema.GroupVersionResource, awsCfg aws.Config, isTaggable bool, recorder record.EventRecorder) error {
	arn, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "arn")
	if arn == "" {
		arn, _, _ = unstructured.NestedString(obj.Object, "status", "atProvider", "accessEntryArn")
	}
	region, _, _ := unstructured.NestedString(obj.Object, "spec", "forProvider", "region")
	if region == "" {
		region = "us-east-1"
	}

	kind := obj.GetKind()
	group := gvr.Group

	if !isTaggable {
		log.Printf("not taggable (no tags field in CRD schema) %s/%s — removing finalizer only", kind, obj.GetName())
	} else {
		// Resources owned by a cross-account ProviderConfig must be tagged with that
		// ProviderConfig's assumed role, not the watcher's own IRSA identity.
		pcName := providerConfigName(obj)
		baseCfg, err := awsCfgFor(ctx, dc, awsCfg, pcName)
		if err != nil {
			assumeRoleErrorTotal.WithLabelValues(pcName).Inc()
			msg := fmt.Sprintf("orphan-tagger could not assume ProviderConfig %s role: %v", pcName, err)
			log.Printf("ERROR %s (%s/%s)", msg, kind, obj.GetName())
			recorder.Event(objRef(obj, gvr, kind), corev1.EventTypeWarning, "OrphanAssumeRoleFailed", msg)
			return fmt.Errorf("resolving credentials for %s/%s: %w", kind, obj.GetName(), err)
		}
		regionCfg := baseCfg.Copy()
		regionCfg.Region = region
		taggingClient := resourcegroupstaggingapi.NewFromConfig(regionCfg)

		log.Printf("tagging %s/%s arn=%s providerConfig=%s with crossplane-orphaned=true", kind, obj.GetName(), arn, pcName)
		tagCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		tagResp, err := taggingClient.TagResources(tagCtx, &resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{arn},
			Tags:            map[string]string{"crossplane-orphaned": "true"},
		})
		if err != nil {
			// Throttling is transient — skip silently and let the next poll retry.
			// Don't count as error or emit event; the MR stays terminating and will be
			// picked up again in 30s.
			if isThrottleError(err) {
				throttledTotal.WithLabelValues(kind, group).Inc()
				log.Printf("throttled tagging %s/%s — will retry next poll cycle", kind, obj.GetName())
				return fmt.Errorf("throttled: %w", err)
			}
			tagErrorTotal.WithLabelValues(kind, group).Inc()
			msg := fmt.Sprintf("orphan-tagger failed to tag ARN %s: %v", arn, err)
			log.Printf("ERROR tagging %s/%s: %v", kind, obj.GetName(), err)
			recorder.Event(objRef(obj, gvr, kind), corev1.EventTypeWarning, "OrphanTagFailed", msg)
			return fmt.Errorf("tagging %s: %w", arn, err)
		}
		// Check per-resource failures in the response body
		if tagResp != nil && len(tagResp.FailedResourcesMap) > 0 {
			for _, fail := range tagResp.FailedResourcesMap {
				if fail.ErrorCode == "InvalidArnException" || fail.ErrorCode == "BadRequestException" || fail.ErrorCode == "UserNotFound" {
					log.Printf("not taggable %s/%s (%s) — removing finalizer only", kind, obj.GetName(), aws.ToString(fail.ErrorMessage))
				} else if fail.ErrorCode == "ThrottledException" || fail.ErrorCode == "ThrottlingException" {
					// Per-resource throttling: keep the finalizer and retry next cycle.
					throttledTotal.WithLabelValues(kind, group).Inc()
					log.Printf("throttled tagging %s/%s — will retry next poll cycle", kind, obj.GetName())
					return fmt.Errorf("throttled tagging %s: %s", arn, aws.ToString(fail.ErrorMessage))
				} else {
					tagErrorTotal.WithLabelValues(kind, group).Inc()
					msg := fmt.Sprintf("orphan-tagger failed to tag ARN %s: %s", arn, aws.ToString(fail.ErrorMessage))
					log.Printf("ERROR tagging %s/%s: %s", kind, obj.GetName(), aws.ToString(fail.ErrorMessage))
					recorder.Event(objRef(obj, gvr, kind), corev1.EventTypeWarning, "OrphanTagFailed", msg)
					return fmt.Errorf("tagging %s: %s", arn, aws.ToString(fail.ErrorMessage))
				}
			}
		} else {
			taggedTotal.WithLabelValues(kind, group).Inc()
			log.Printf("tagged %s/%s — removing finalizer", kind, obj.GetName())
		}
	}

	// Re-fetch live finalizers to avoid stale-state patch rejection.
	live, err := dc.Resource(gvr).Namespace(obj.GetNamespace()).Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-fetching %s/%s: %w", kind, obj.GetName(), err)
	}

	current := live.GetFinalizers()
	updated := make([]string, 0, len(current))
	for _, f := range current {
		if f != orphanFinalizer {
			updated = append(updated, f)
		}
	}
	log.Printf("removing finalizer from %s/%s (%v → %v)", kind, obj.GetName(), current, updated)

	patchBytes, err := marshalFinalizers(updated)
	if err != nil {
		return err
	}
	_, err = dc.Resource(gvr).Namespace(obj.GetNamespace()).Patch(
		ctx, obj.GetName(),
		"application/merge-patch+json",
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		msg := fmt.Sprintf("orphan-tagger failed to remove finalizer: %v", err)
		log.Printf("ERROR removing finalizer from %s/%s: %v", kind, obj.GetName(), err)
		recorder.Event(objRef(obj, gvr, kind), corev1.EventTypeWarning, "OrphanFinalizerRemoveFailed", msg)
		return fmt.Errorf("removing finalizer from %s/%s: %w", kind, obj.GetName(), err)
	}

	mrTerminating.DeletePartialMatch(prometheus.Labels{"name": obj.GetName(), "kind": kind, "group": group})
	mrFinalizerCount.DeleteLabelValues(obj.GetName(), kind, group)
	log.Printf("done: %s/%s", kind, obj.GetName())
	return nil
}

func discoverAWSGroups(ctx context.Context, dc dynamic.Interface) ([]schema.GroupVersionResource, map[string]bool, error) {
	list, err := dc.Resource(schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}

	var gvrs []schema.GroupVersionResource
	taggableKinds := make(map[string]bool)
	crdInfo.Reset()

	for _, item := range list.Items {
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		if !strings.Contains(group, ".aws.upbound.io") {
			continue
		}

		kind, _, _ := unstructured.NestedString(item.Object, "spec", "names", "kind")
		resource, _, _ := unstructured.NestedString(item.Object, "spec", "names", "plural")
		versions, _, _ := unstructured.NestedSlice(item.Object, "spec", "versions")

		// Prefer storage version; fall back to first served version.
		version := ""
		var storageVersionObj map[string]interface{}
		for _, v := range versions {
			vm, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			if storage, _, _ := unstructured.NestedBool(vm, "storage"); storage {
				version, _, _ = unstructured.NestedString(vm, "name")
				storageVersionObj = vm
				break
			}
		}
		if version == "" {
			for _, v := range versions {
				vm, ok := v.(map[string]interface{})
				if !ok {
					continue
				}
				if served, _, _ := unstructured.NestedBool(vm, "served"); served {
					version, _, _ = unstructured.NestedString(vm, "name")
					storageVersionObj = vm
					break
				}
			}
		}
		if version == "" {
			continue
		}

		// Check if spec.forProvider.properties.tags exists in the storage version schema
		if storageVersionObj != nil {
			_, hasTags, _ := unstructured.NestedFieldNoCopy(storageVersionObj,
				"schema", "openAPIV3Schema", "properties", "spec",
				"properties", "forProvider", "properties", "tags")
			taggableKinds[kind] = hasTags
			taggableStr := "false"
			if hasTags {
				taggableStr = "true"
			}
			crdInfo.WithLabelValues(kind, group, taggableStr).Set(1)
		}

		gvrs = append(gvrs, schema.GroupVersionResource{
			Group: group, Version: version, Resource: resource,
		})
	}
	return gvrs, taggableKinds, nil
}

// cfgCache caches the resolved aws.Config per ProviderConfig name so we hit STS
// once per ProviderConfig instead of once per resource per poll cycle.
// Credentials inside are wrapped in aws.NewCredentialsCache, which refreshes them
// before expiry, so a long-lived entry stays valid.
var cfgCache sync.Map

// providerConfigName returns the ProviderConfig an MR is bound to. Crossplane
// defaults to "default" when spec.providerConfigRef is unset.
func providerConfigName(obj *unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "providerConfigRef", "name")
	if name == "" {
		return "default"
	}
	return name
}

// awsCfgFor resolves the credentials to use for an MR's ProviderConfig. When that
// ProviderConfig declares an assumeRoleChain (cross-account resources such as the
// drp-kms keys or per-GPE-cell resources), each role is assumed in order starting
// from the watcher's own IRSA identity — mirroring what provider-aws itself does —
// so the tag call lands in the account that actually owns the resource.
func awsCfgFor(ctx context.Context, dc dynamic.Interface, base aws.Config, pcName string) (aws.Config, error) {
	if v, ok := cfgCache.Load(pcName); ok {
		return v.(aws.Config), nil
	}

	pc, err := dc.Resource(providerConfigGVR).Get(ctx, pcName, metav1.GetOptions{})
	if err != nil {
		return base, fmt.Errorf("getting ProviderConfig %s: %w", pcName, err)
	}

	chain, _, _ := unstructured.NestedSlice(pc.Object, "spec", "assumeRoleChain")
	if len(chain) == 0 {
		// Older single-role form.
		if arn, _, _ := unstructured.NestedString(pc.Object, "spec", "assumeRole", "roleARN"); arn != "" {
			chain = []interface{}{map[string]interface{}{"roleARN": arn}}
		}
	}

	cfg := base.Copy()
	for _, step := range chain {
		m, ok := step.(map[string]interface{})
		if !ok {
			continue
		}
		roleARN, _, _ := unstructured.NestedString(m, "roleARN")
		if roleARN == "" {
			continue
		}
		externalID, _, _ := unstructured.NestedString(m, "externalID")

		stsClient := sts.NewFromConfig(cfg)
		next := cfg.Copy()
		next.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, roleARN,
			func(o *stscreds.AssumeRoleOptions) {
				o.RoleSessionName = "orphan-tagger"
				if externalID != "" {
					o.ExternalID = aws.String(externalID)
				}
			}))
		cfg = next
	}

	if len(chain) > 0 {
		// Fail fast here rather than surfacing an opaque AccessDenied from TagResources.
		if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
			return base, fmt.Errorf("assuming role chain for ProviderConfig %s: %w", pcName, err)
		}
		log.Printf("resolved ProviderConfig %s via %d-step assumeRoleChain", pcName, len(chain))
	}

	cfgCache.Store(pcName, cfg)
	return cfg, nil
}

// isThrottleError returns true for transient AWS rate-limit errors that should
// be retried on the next poll cycle rather than counted as permanent failures.
func isThrottleError(err error) bool {
	type throttleChecker interface{ IsThrottle() bool }
	var t throttleChecker
	if errors.As(err, &t) && t.IsThrottle() {
		return true
	}
	// Fallback: check the error string for known throttle codes. Also covers the
	// retry wrapper ("exceeded maximum number of attempts, N") and adaptive-mode
	// quota exhaustion, which surface once retries are used up.
	msg := err.Error()
	return strings.Contains(msg, "ThrottledException") ||
		strings.Contains(msg, "ThrottlingException") ||
		strings.Contains(msg, "Throttling") ||
		strings.Contains(msg, "Rate exceeded") ||
		strings.Contains(msg, "exceeded maximum number of attempts") ||
		strings.Contains(msg, "retry quota exceeded") ||
		strings.Contains(msg, "RequestLimitExceeded") ||
		strings.Contains(msg, "TooManyRequestsException")
}

// objRef builds a runtime.Object usable by EventRecorder from an unstructured MR.
// Dynamic client list items often have empty kind/apiVersion; we set them from the
// known GVR + kind so the event reference is not malformed and silently dropped.
// Namespace is intentionally NOT overridden: for cluster-scoped resources the
// recorder stores the event in "default" but keeps involvedObject.namespace=""
// so kubectl describe can match it.
func objRef(obj *unstructured.Unstructured, gvr schema.GroupVersionResource, kind string) runtime.Object {
	ref := obj.DeepCopy()
	if ref.GetKind() == "" {
		ref.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gvr.Group,
			Version: gvr.Version,
			Kind:    kind,
		})
	}
	return ref
}

func marshalFinalizers(finalizers []string) ([]byte, error) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": finalizers,
		},
	}}
	b, err := u.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshaling finalizers patch: %w", err)
	}
	return b, nil
}
