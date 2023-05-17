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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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
	home, _ := os.UserHomeDir()

	kubeconfig := ""
	if _, err := os.Stat(filepath.Join(home, ".kube", "config")); err == nil {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatal(err)
	}

	client, err := kubernetes.NewForConfig(config)
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

	// Sort Ingress objects by their name
	sort.Slice(ingresses.Items, func(i, j int) bool {
		return ingresses.Items[i].Name < ingresses.Items[j].Name
	})

	// Iterate over ingresses and extract annotations
	sites := make([]Site, 0)
	for _, ingress := range ingresses.Items {
		sites = append(sites, parseIngress(ingress))
	}

	// Sort by prio
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].Priority < sites[j].Priority
	})

	fmt.Printf("Found %d sites: +%v\n", len(sites), sites)

	return sites
}

func parseIngress(ingress networkv1.Ingress) Site {
	annotations := ingress.GetAnnotations()

	// Create Site from ingress annotations
	site := Site{
		Name: annotations["reloader.homer/name"],
		Url:  annotations["reloader.homer/url"],
	}
	// Parse group
	group, ok := annotations["reloader.homer/group"]
	if ok {
		site.Group = group
	}
	// Parse subtitle
	subtitle, ok := annotations["reloader.homer/subtitle"]
	if ok {
		site.Subtitle = subtitle
	}
	// Parse logo
	logo, ok := annotations["reloader.homer/logo"]
	if ok {
		site.Logo = logo
	}
	// Parse tag
	tag, ok := annotations["reloader.homer/tag"]
	if ok {
		site.Tag = tag
	}
	// Parse priority
	prio, err := strconv.Atoi(annotations["reloader.homer/prio"])
	if err != nil {
		site.Priority = 999
	} else {
		site.Priority = prio
	}

	// Check if ingress has a tls section
	scheme := "http"
	if len(ingress.Spec.TLS) > 0 {
		scheme = "https"
	}

	// Get URL from first host in ingress object
	if len(ingress.Spec.Rules) > 0 {
		site.Url = fmt.Sprintf("%s://%s", scheme, ingress.Spec.Rules[0].Host)
	}
	return site
}

func generateHomerConfig(t string, sites []Site) (string, string, error) {
	tmpl, err := template.New("config").Parse(t)
	if err != nil {
		return "", "", err
	}

	// Execute template
	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, sites)
	if err != nil {
		return "", "", err
	}

	// calculate checksum
	checksum := fmt.Sprintf("%x", md5.Sum(buf.Bytes()))
	return buf.String(), checksum, nil
}

func createOrUpdateConfigMap(clientset *kubernetes.Clientset, namespace, name string, data map[string]string) error {
	_, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		_, err := clientset.CoreV1().ConfigMaps(namespace).Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Data: data,
		}, metav1.CreateOptions{})
		return err
	}
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Data: data,
	}, metav1.UpdateOptions{})
	return err
}

func annotateDeployment(clientset *kubernetes.Clientset, namespace string, deployment string, checksum string) error {
	d, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deployment, metav1.GetOptions{})
	if err != nil {
		log.Println("Unable to get deployment")
		return err
	}
	d.Spec.Template.ObjectMeta.Annotations["checksum/config"] = checksum
	_, err = clientset.AppsV1().Deployments(namespace).Update(context.TODO(), d, metav1.UpdateOptions{})
	if err != nil {
		log.Println("Unable to update deployment")
		return err
	}
	return nil
}
