package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

var (
	app = kingpin.New("alertbit", "A bridge between fluentbit and alertmanager.")

	addr        = app.Flag("addr", "HTTP server listen address").Default(":8090").String()
	amAddr      = app.Flag("alertmanager-addr", "Alertmanager Host").Default("localhost:9093").String()
	labels      = app.Flag("labels", "Extra labels").StringMap()
	ignoreLabel = app.Flag("ignore-label", "Label to ignore").Strings()
	ttl         = app.Flag("ttl", "Alerts time to live").Default("300s").Duration()

	now = app.Flag("now", "Use current time").Bool()
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

	http.HandleFunc("/-/ready", readyHandler)
	http.HandleFunc("/-/healthy", healthyHandler)
	http.HandleFunc("/", postHandler)

	fmt.Printf("Listening on %s\n", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func healthyHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
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

	var alerts []Alert

	for _, obj := range inputObjects {
		date, ok := obj["date"].(float64)
		if !ok {
			continue
		}
		if *now {
			date = float64(time.Now().Unix())
		}
		// Convert epoch to RFC3339
		startsAt := time.Unix(int64(math.Round(date)), 0).Format(time.RFC3339)
		endsAt := time.Unix(int64(math.Round(date)), 0).Add(*ttl).Format(time.RFC3339)

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

		alert := Alert{
			Labels:       lbls,
			Annotations:  anns,
			StartsAt:     startsAt,
			EndsAt:       endsAt,
			GeneratorURL: "http://bridge.invalid",
		}

		alerts = append(alerts, alert)
	}

	// Marshal alerts into JSON
	alertsJSON, err := json.Marshal(alerts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling alerts: %v\n", err)
		http.Error(w, "Error marshaling alerts", http.StatusInternalServerError)
		return
	}

	// Post alerts to the specified URL
	resp, err := http.Post(fmt.Sprintf("http://%s/api/v2/alerts", *amAddr), "application/json", bytes.NewBuffer(alertsJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error posting alerts: %v\n", err)
		http.Error(w, "Error posting alerts", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error copying response body: %v\n", err)
	}

	w.WriteHeader(resp.StatusCode)
}
