package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"

	policyv1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cloud "k8s.io/cloud-provider/node/helpers"
)

const (
	lifetimeAnnotation string = "pod.kubernetes.io/lifetime"
)

var (
	metricPodsReaped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pod_reaper_reaped",
			Help: "Number of reaped pods.",
		},
		[]string{
			"namespace",
			"method",
		},
	)
	metricPods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pod_reaper_detected",
			Help: "Number of pods watching.",
		},
		[]string{
			"namespace",
			"kind",
		},
	)
	metricNodes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_reaper_detected",
			Help: "Number of nodes watching.",
		},
		[]string{
			"nodegroups",
			"nodes",
		},
	)
)

type NodeGroup struct {
	Spot    int
	Tainted int
	Total   int
}

func main() {
	log.Level = log.LevelDebug

	log.Infof("Pod reaper smiles at all pods; all a pod can do is smile back.")
	log.Infof("You can run but you can't hide!\n")

	var config *rest.Config
	var err error
	var maxReaperCount = maxReaperCountPerRun()
	var (
		evict        = evict()
		reapEvicted  = reapEvictedPods()
		runAsCronJob = cronJob()
	)

	if !reapEvicted {
		log.Debugf("REAP_EVICTED_PODS not set. Not reaping evicted pods.")
	}

	if remoteExec() {
		log.Debug("Loading kubeconfig from in cluster config")
		config, err = rest.InClusterConfig()
	} else {
		var kubeconfig *string
		if home := homeDir(); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}

		flag.Parse()
		log.Infof("Loading kubeconfig from %s\n", *kubeconfig)

		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}

	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// register metrics
	prometheus.MustRegister(metricPods)
	prometheus.MustRegister(metricPodsReaped)

	// metrics server
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok\n")
	})
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		err = http.ListenAndServe(":8080", nil)
		if err != nil {
			panic("failed to start server at port 8080")
		}
	}()

	reaperNamespaces := namespaces()

	for {

		reapPods(clientset, reaperNamespaces, maxReaperCount, evict, reapEvicted)

		reapNodes(clientset)

		if runAsCronJob {
			break
		}
		log.Infof("Now sleeping for %d seconds", int(sleepDuration().Seconds()))
		time.Sleep(sleepDuration())
	}
}

func remoteExec() bool {
	if val, ok := os.LookupEnv("REMOTE_EXEC"); ok {
		boolVal, err := strconv.ParseBool(val)
		if err == nil {
			return boolVal
		} else {
			panic("REMOTE_EXEC var incorrectly set")
		}
	}
	return false
}

func getNodeLifeTime() string {
	i := os.Getenv("NODE_LIFE_TIME")
	return i
}

func maxReaperCountPerRun() int {
	i, err := strconv.Atoi(os.Getenv("MAX_REAPER_COUNT_PER_RUN"))
	if err != nil {
		i = 30
	}
	return i
}

func reapEvictedPods() bool {
	if val, ok := os.LookupEnv("REAP_EVICTED_PODS"); ok {
		boolVal, err := strconv.ParseBool(val)
		if err == nil {
			return boolVal
		}
	}
	return false
}

func cronJob() bool {
	if val, ok := os.LookupEnv("CRON_JOB"); ok {
		boolVal, err := strconv.ParseBool(val)
		if err == nil {
			return boolVal
		}
	}
	return false
}

func sleepDuration() time.Duration {
	if h := os.Getenv("REAPER_INTERVAL_IN_SEC"); h != "" {
		s, _ := strconv.Atoi(h)
		return time.Duration(s) * time.Second
	}
	return 60 * time.Second
}

func namespaces() []string {
	if h := os.Getenv("REAPER_NAMESPACES"); h != "" {
		namespaces := strings.Split(h, ",")
		if len(namespaces) == 1 && strings.ToLower(namespaces[0]) == "all" {
			return []string{metav1.NamespaceAll}
		}
		return namespaces
	}
	return []string{}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func evict() bool {
	if val, ok := os.LookupEnv("EVICT"); ok {
		boolVal, err := strconv.ParseBool(val)
		if err == nil {
			return boolVal
		}
	}
	return false
}

func reapPods(clientset *kubernetes.Clientset, reaperNamespaces []string, maxReaperCount int, evict bool, reapEvicted bool) {
	if len(reaperNamespaces) == 0 {
		log.Infof("No namespaces to monitor")
		return
	}
	for _, ns := range reaperNamespaces {
		pods, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}

		log.Infof("Checking %d pods in namespace %s\n", len(pods.Items), ns)
		podsTracking := 0
		podsKilled := 0

		for _, v := range pods.Items {
			if val, ok := v.Annotations[lifetimeAnnotation]; ok {
				log.Debugf("pod %s : Found annotation %s with value %s\n", v.Name, lifetimeAnnotation, val)
				podsTracking++
				lifetime, _ := time.ParseDuration(val)
				if lifetime == 0 {
					log.Debugf("pod %s : provided value %s is incorrect\n", v.Name, val)
				} else if podsKilled < maxReaperCount {
					log.Debugf("pod %s : %s\n", v.Name, v.CreationTimestamp)
					currentLifetime := time.Since(v.CreationTimestamp.Time)
					if currentLifetime > lifetime {
						var err error
						if evict {
							log.Infof("pod %s : pod is past its lifetime and will be evicted\n", v.Name)
							err = clientset.CoreV1().Pods(v.Namespace).Evict(context.TODO(), &policyv1.Eviction{
								ObjectMeta:    metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name},
								DeleteOptions: &metav1.DeleteOptions{},
							})
						} else {
							log.Infof("pod %s : pod is past its lifetime and will be killed.\n", v.Name)
							err = clientset.CoreV1().Pods(v.Namespace).Delete(context.TODO(), v.Name, metav1.DeleteOptions{})
						}
						if err != nil {
							log.Infof("unable to reap pod %s : %s", v.Name, err.Error())
						} else {
							log.Infof("pod %s : pod reaped.\n", v.Name)
							podsKilled++
						}
					}
				} else {
					log.Debugf("pod %s : max %d pods killed\n", v.Name, maxReaperCount)
				}
			}

			if reapEvicted && strings.Contains(v.Status.Reason, "Evicted") {
				log.Debugf("pod %s : pod is evicted and needs to be deleted", v.Name)
				err := clientset.CoreV1().Pods(v.Namespace).Delete(context.TODO(), v.Name, metav1.DeleteOptions{})
				if err != nil {
					panic(err.Error())
				}
				log.Infof("pod %s : pod killed.\n", v.Name)
				podsKilled++
			}
		}

		log.Infof("Killed %d Old/Evicted Pods.", podsKilled)
		metricPods.WithLabelValues(ns, "ignoring").Set(float64(len(pods.Items) - podsTracking))
		metricPods.WithLabelValues(ns, "tracking").Set(float64(podsTracking))
		metricPodsReaped.WithLabelValues(ns, "killed").Add(float64(podsKilled))
	}
}

func reapNodes(clientset *kubernetes.Clientset) {
	nodeLifeTime := getNodeLifeTime()
	if len(nodeLifeTime) == 0 {
		log.Infof("\nNode reaper is disabled.")
		return
	}
	lifetime, err := time.ParseDuration(getNodeLifeTime())
	if err != nil {
		log.Errorf("\nFailed to process NODE_LIFE_TIME = %v", nodeLifeTime)
		return
	}

	var taint = &v1.Taint{
		Key:    "ci_node",
		Effect: v1.TaintEffectNoSchedule,
		Value:  "Disable",
	}

	nodes, _ := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	// count nodes in nodeGroup
	nGroups := make(map[string]NodeGroup)
	for _, node := range nodes.Items {
		var nodeGroup string
		var ng NodeGroup
		for k, v := range node.Labels {
			if k == "node-lifecycle" && v == "spot" {
				ng.Spot = 1
			}
			if k == "ci_node" && v == "Disable:NoSchedule" {
				ng.Tainted = 1
			}
			if k == "alpha.eksctl.io/nodegroup-name" {
				nodeGroup = v
			}
		}
		_, ok := nGroups[nodeGroup]
		if ok {
			ng.Spot += nGroups[nodeGroup].Spot
			ng.Tainted += nGroups[nodeGroup].Tainted
			ng.Total += nGroups[nodeGroup].Total
		}
		nGroups[nodeGroup] = ng
	}
	// expose metrics
	for ngName, ngValue := range nGroups {
		metricNodes.WithLabelValues(ngName, "spot").Set(float64(ngValue.Spot))
		metricNodes.WithLabelValues(ngName, "tainted").Set(float64(ngValue.Tainted))
		metricNodes.WithLabelValues(ngName, "total").Set(float64(ngValue.Total))
	}
	// apply taint
	for _, node := range nodes.Items {
		spot := false
		currentLifetime := time.Since(node.CreationTimestamp.Time)
		if currentLifetime < lifetime {
			continue
		}
		for k, v := range node.Labels {
			if k == "node-lifecycle" && v == "spot" {
				spot = true
			}
		}
		if !spot {
			continue
		}
		err := cloud.AddOrUpdateTaintOnNode(clientset, node.Name, taint)
		if err != nil {
			log.Errorf("\nfailed to apply shutdown taint to node %v, it may have been deleted.", node.Name)
		}
		log.Debugf("\nDisable node %v", node.Name)
	}
}
