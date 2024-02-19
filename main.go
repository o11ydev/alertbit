package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	labelspkg "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/notifier"
)

var (
	app = kingpin.New("alertbit", "A bridge between fluentbit and alertmanager.")

	addr            = app.Flag("addr", "HTTP server listen address").Default(":8090").String()
	cfgFile         = app.Flag("sd-config", "Service Discovery").Default("config.yml").String()
	labels          = app.Flag("labels", "Extra labels").StringMap()
	ignoreLabel     = app.Flag("ignore-label", "Label to ignore").Strings()
	ttl             = app.Flag("ttl", "Alerts time to live").Default("300s").Duration()
	notifierManager *notifier.Manager

	now = app.Flag("now", "Use current time").Bool()

	targets map[string][]*targetgroup.Group
)

type InputObject map[string]interface{}

type Alert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

func main() {
	kingpin.MustParse(app.Parse(os.Args[1:]))

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/-/ready", readyHandler)
	http.HandleFunc("/-/healthy", healthyHandler)
	http.HandleFunc("/", postHandler)

	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	sdMetrics, err := discovery.CreateAndRegisterSDMetrics(prometheus.DefaultRegisterer)
	if err != nil {
		level.Error(logger).Log("msg", "failed to register service discovery metrics", "err", err)
		os.Exit(1)
	}
	discoveryManagerNotify := discovery.NewManager(context.TODO(), log.With(logger, "component", "discovery manager scrape"), prometheus.DefaultRegisterer, sdMetrics, discovery.Name("scrape"))
	if discoveryManagerNotify == nil {
		os.Exit(1)
	}

	cfg, err := config.LoadFile(*cfgFile, false, false, logger)

	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	c := make(map[string]discovery.Configs)
	for k, v := range cfg.AlertingConfig.AlertmanagerConfigs.ToMap() {
		c[k] = v.ServiceDiscoveryConfigs
	}
	err = discoveryManagerNotify.ApplyConfig(c)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	go discoveryManagerNotify.Run()

	notifierOpt := notifier.Options{
		Registerer:    prometheus.DefaultRegisterer,
		QueueCapacity: 3000,
	}
	notifierManager = notifier.NewManager(&notifierOpt, log.With(logger, "component", "notifier"))
	notifierManager.ApplyConfig(cfg)
	go notifierManager.Run(discoveryManagerNotify.SyncCh())

	fmt.Printf("Listening on %s\n", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		logger.Log("error", fmt.Sprintf("Error starting server: %v", err))
	}

}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	if notifierManager == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func healthyHandler(w http.ResponseWriter, r *http.Request) {
	readyHandler(w, r)
}

func postHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is accepted", http.StatusMethodNotAllowed)
		return
	}

	var inputObjects []InputObject

	// Read input from stdin or file
	inputData, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}

	// Parse input JSON
	err = json.Unmarshal(inputData, &inputObjects)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing input JSON: %v\n", err)
		http.Error(w, "Error parsing input JSON", http.StatusInternalServerError)
		return
	}

	var alerts []*notifier.Alert

	for _, obj := range inputObjects {
		date, ok := obj["date"].(float64)
		if !ok {
			continue
		}
		if *now {
			date = float64(time.Now().Unix())
		}
		// Convert epoch to RFC3339
		startsAt := time.Unix(int64(math.Round(date)), 0)
		endsAt := time.Unix(int64(math.Round(date)), 0).Add(*ttl)

		// Compute checksum as log_id
		jsonBytes, _ := json.Marshal(obj)
		checksum := sha256.Sum256(jsonBytes)
		logID := hex.EncodeToString(checksum[:])
		if len(logID) > 10 {
			logID = logID[:8]
		}

		lbls := map[string]string{
			"__log_id": logID,
		}
		anns := make(map[string]string)
		if labels != nil {
			for k, v := range *labels {
				lbls[k] = v
			}
		}
		for k, v := range obj {
			if k == "date" {
				continue
			}
			fnd := true
			// Always add fields as annotations.
			anns[k] = fmt.Sprintf("%v", v)
			if ignoreLabel != nil {
				for _, l := range *ignoreLabel {
					if k == l {
						fnd = false
						continue
					}
				}
			}
			if !fnd {
				continue
			}
			lbls[k] = fmt.Sprintf("%v", v)
		}

		alert := &notifier.Alert{
			Labels:       labelspkg.FromMap(lbls),
			Annotations:  labelspkg.FromMap(anns),
			StartsAt:     startsAt,
			EndsAt:       endsAt,
			GeneratorURL: "http://bridge.invalid",
		}

		alerts = append(alerts, alert)
	}

	notifierManager.Send(alerts...)

	w.WriteHeader(http.StatusOK)
}
