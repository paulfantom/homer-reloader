package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"text/template"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	metricPrefix = "homer_reloader"
)

type Site struct {
	Name     string `json:"name"`               // Maps to `reloader.homer/name` ingress annotation
	Url      string `json:"url"`                // Maps to ingress host
	Group    string `json:"group"`              // Maps to `reloader.homer/group` ingress annotation
	Subtitle string `json:"subtitle,omitempty"` // Maps to `reloader.homer/subtitle` ingress annotation
	Logo     string `json:"logo,omitempty"`     // Maps to `reloader.homer/logo` ingress annotation
	Tag      string `json:"tag,omitempty"`      // Maps to `reloader.homer/tag` ingress annotation
	Priority int    `json:"priority,omitempty"` // Maps to `reloader.homer/prio` ingress annotation
}

func main() {
	homerNamespace := flag.String("homer-namespace", "homer", "homer deployment namespace")
	homerDeployment := flag.String("homer-deployment", "homer", "homer deployment name")
	homerConfigMap := flag.String("homer-configmap", "homer-configmap", "the config map containing the homer configuration")
	templateConfigMap := flag.String("template-configmap", "homer-template", "the config map containing the template for homer configuration")
	flag.Parse()

	reg := prometheus.NewRegistry()
	reloadsTotal := promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Name:      "reloads_total",
			Namespace: metricPrefix,
			Help:      "Tracks the number of requested reloads.",
		}, []string{"deployment"},
	)
	reloadsErrorsTotal := promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Name:      "reloads_errors_total",
			Namespace: metricPrefix,
			Help:      "Tracks the number of unsuccessful reloads.",
		}, []string{"deployment"},
	)
	lastReloadError := promauto.With(reg).NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "last_reload_error",
			Namespace: metricPrefix,
			Help:      "Whether the last reload resulted in an error (1 for error, 0 for success)",
		}, []string{"deployment"},
	)

	// Add Go module build info.
	reg.MustRegister(
		collectors.NewBuildInfoCollector(),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Initialize metrics
	reloadsTotal.WithLabelValues(*homerDeployment)
	lastReloadError.WithLabelValues(*homerDeployment)
	reloadsErrorsTotal.WithLabelValues(*homerDeployment)

	// Get Kubernetes client
	client, err := getKubernetesClient()
	if err != nil {
		log.Fatal(err)
	}

	// Watch ingresses
	watcher, err := client.
		NetworkingV1().
		Ingresses("").
		Watch(
			context.Background(),
			metav1.ListOptions{
				LabelSelector: "reloader.homer/enabled==true",
			},
		)
	if err != nil {
		log.Fatal(err)
	}

	mutex := &sync.Mutex{}

	wg := &sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			event, open := <-watcher.ResultChan()
			if !open {
				return
			}

			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				mutex.Lock()
				reloadsTotal.WithLabelValues(*homerDeployment).Inc()
				err := reload(client, *homerNamespace, *homerDeployment, *homerConfigMap, *templateConfigMap)
				if err != nil {
					lastReloadError.WithLabelValues(*homerDeployment).Set(1.0)
					log.Println(err)
				} else {
					lastReloadError.WithLabelValues(*homerDeployment).Set(0.0)
					reloadsErrorsTotal.WithLabelValues(*homerDeployment).Inc()
				}
				mutex.Unlock()
			}
		}
	}()

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	log.Fatal(http.ListenAndServe(":9333", nil))

	wg.Wait()
}

func getKubernetesClient() (*kubernetes.Clientset, error) {
	// Find kubeconfig file
	kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
	if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
		kubeconfig = "" // Use in-cluster configuration if kubeconfig doesn't exist
	}

	// Build Kubernetes client config
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	// Create Kubernetes client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func reload(client *kubernetes.Clientset, homerNamespace, homerDeployment, homerConfigMap, templateConfigMap string) error {
	// get ingresses and parse annotations
	sites := getSites(client)

	// Get template config map data
	// read template configmap
	cm, err := client.CoreV1().ConfigMaps(homerNamespace).Get(context.Background(), templateConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	template := cm.Data["config.yml.tpl"]

	// Generate site configuration
	config, checksum, err := generateHomerConfig(template, sites)
	log.Println("checksum: ", checksum)
	if err != nil {
		return err
	}

	// Load new site config into a config map
	// update homer configmap
	err = createOrUpdateConfigMap(client, homerNamespace, homerConfigMap, map[string]string{
		"config.yml": string(config),
	})
	if err != nil {
		return err
	}

	// Annotate deployment with checksum
	err = annotateDeployment(client, homerNamespace, homerDeployment, checksum)
	if err != nil {
		return err
	}

	return nil
}

func getSites(clientset *kubernetes.Clientset) []Site {
	ingresses, err := clientset.NetworkingV1().Ingresses("").List(context.Background(), metav1.ListOptions{
		LabelSelector: "reloader.homer/enabled==true",
	})
	if err != nil {
		log.Fatal(err)
	}

	// Create a buffered channel for ingresses
	ingressChan := make(chan networkv1.Ingress, len(ingresses.Items))

	// Process ingresses concurrently
	var wg sync.WaitGroup
	for _, ingress := range ingresses.Items {
		wg.Add(1)
		go func(i networkv1.Ingress) {
			defer wg.Done()
			ingressChan <- i
		}(ingress)
	}

	go func() {
		wg.Wait()
		close(ingressChan)
	}()

	// Collect parsed sites from ingresses
	sites := make([]Site, 0)
	for ingress := range ingressChan {
		sites = append(sites, parseIngress(ingress))
	}

	// Sort by name
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].Name < sites[j].Name
	})

	// Sort by priority
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].Priority < sites[j].Priority
	})

	return sites
}

func parseIngress(ingress networkv1.Ingress) Site {
	annotations := ingress.GetAnnotations()

	// Define a map of annotation keys and corresponding struct field names
	annotationKeys := map[string]string{
		"reloader.homer/name":     "Name",
		"reloader.homer/url":      "Url",
		"reloader.homer/group":    "Group",
		"reloader.homer/subtitle": "Subtitle",
		"reloader.homer/logo":     "Logo",
		"reloader.homer/tag":      "Tag",
		//"reloader.homer/prio":     "Priority",  Priority is handled separately as reflect cannot set int fields
	}

	// Create Site from ingress annotations
	site := Site{}

	for annotationKey, fieldName := range annotationKeys {
		if value, ok := annotations[annotationKey]; ok {
			reflect.ValueOf(&site).Elem().FieldByName(fieldName).SetString(value)
		}
	}

	// Check if ingress has a TLS section
	scheme := "http"
	if len(ingress.Spec.TLS) > 0 {
		scheme = "https"
	}

	// Get URL from the first host in the ingress object
	if len(ingress.Spec.Rules) > 0 {
		site.Url = fmt.Sprintf("%s://%s", scheme, ingress.Spec.Rules[0].Host)
	}

	// Parse priority
	if prioStr, ok := annotations["reloader.homer/prio"]; ok {
		if prio, err := strconv.Atoi(prioStr); err == nil {
			site.Priority = prio
		} else {
			site.Priority = 999
		}
	}

	return site
}

func generateHomerConfig(t string, sites []Site) (string, string, error) {
	tmpl, err := template.New("config").Parse(t)
	if err != nil {
		return "", "", err
	}

	// Initialize and reset buffer
	buf := new(bytes.Buffer)
	buf.Reset()

	// Execute template
	err = tmpl.Execute(buf, sites)
	if err != nil {
		return "", "", err
	}

	// calculate checksum
	checksum := fmt.Sprintf("%x", md5.Sum(buf.Bytes()))
	return buf.String(), checksum, nil
}

func createOrUpdateConfigMap(clientset *kubernetes.Clientset, namespace, name string, data map[string]string) error {
	configMaps := clientset.CoreV1().ConfigMaps(namespace)
	existingConfigMap, err := configMaps.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap doesn't exist, create a new one
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Data: data,
			}
			_, err = configMaps.Create(context.Background(), configMap, metav1.CreateOptions{})
			return err
		}
		return err
	}

	// ConfigMap exists, update it
	existingConfigMap.Data = data
	_, err = configMaps.Update(context.Background(), existingConfigMap, metav1.UpdateOptions{})
	return err
}

func annotateDeployment(clientset *kubernetes.Clientset, namespace string, deployment string, checksum string) error {
	patch := []byte(fmt.Sprintf(`{"spec": {"template": {"metadata": {"annotations": {"checksum/config": "%s"}}}}`, checksum))
	_, err := clientset.AppsV1().Deployments(namespace).Patch(context.TODO(), deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}
