package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

var addr, amAddr string

type InputObject map[string]interface{}

type Alert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

func main() {
	flag.StringVar(&addr, "addr", ":8090", "HTTP server listen address")
	flag.StringVar(&amAddr, "alertmanager-addr", "localhost:9093", "Alertmanager Host")
	flag.Parse()

	http.HandleFunc("/", postHandler) // Set the handler for POST requests

	fmt.Printf("Listening on %s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
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
		// Convert epoch to RFC3339
		startsAt := time.Unix(int64(math.Round(date)), 0).Format(time.RFC3339)
		endsAt := time.Unix(int64(math.Round(date+300)), 0).Format(time.RFC3339)

		// Compute checksum as log_id
		jsonBytes, _ := json.Marshal(obj)
		checksum := sha256.Sum256(jsonBytes)
		logID := hex.EncodeToString(checksum[:])

		anns := make(map[string]string)
		for k, v := range obj {
			if k == "date" {
				continue
			}
			anns[k] = fmt.Sprintf("%v", v)
		}

		alert := Alert{
			Labels: map[string]string{
				"log_id": logID,
			},
			Annotations:  anns,
			StartsAt:     startsAt,
			EndsAt:       endsAt,
			GeneratorURL: "bridge",
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
	resp, err := http.Post(fmt.Sprintf("http://%s/api/v2/alerts", amAddr), "application/json", bytes.NewBuffer(alertsJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error posting alerts: %v\n", err)
		http.Error(w, "Error posting alerts", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	w.WriteHeader(http.StatusNoContent)
}
