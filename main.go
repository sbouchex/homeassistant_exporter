package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jasonlvhit/gocron"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var (
	listeningAddress         = flag.String("web.listen-address", ":9103", "Address on which to expose metrics and web interface.")
	metricsPath              = flag.String("web.telemetry-path", "/metrics", "Path under which to expose Prometheus metrics.")
	homeAssistantQuery       = flag.String("ha.query", "http://127.0.0.1:8123/api/states", "Home Assistant Query")
	homeAssistantAPI         = flag.String("ha.api", "", "Home Assistant API")
	homeAssistantPollingRate = flag.Uint64("ha.rate", 300, "Home Assistant Polling Rate")
	homeAssistantTest        = flag.Bool("ha.test", false, "Home Assistant Test")
	mappingfile              = flag.String("mapping.file", "mappings.json", "Mapping File")
	verbose                  = flag.Bool("verbose", false, "Verbose mode")
	lastPush                 = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "homeassistant_last_push_timestamp_seconds",
			Help: "Unix timestamp of the last received metrics push in seconds.",
		},
	)
	mappings = map[string]MappingEntry{}
)

type MappingEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Mapping struct {
	EntityId     string `json:"entity_id"`
	MappingEntry string `json:"metric"`
}

type Entity struct {
	EntityId     string `json:"entity_id"`
	FriendlyName string `json:"friendly_name"`
	State        string `json:"state"`
	LastUpdated  string `json:"last_updated"`
}

func metricName(m Entity, name string) string {
	result := "homeassistant"
	result += "_"
	result += fmt.Sprintf("%s", name)
	return result
}

func metricHelp(m Entity, name string) string {
	return fmt.Sprintf("homeassistant exporter: Name: '%s'", name)
}

func metricType(e Entity, m MappingEntry) prometheus.ValueType {
	if m.Type == "counter" || m.Type == "derive" {
		return prometheus.CounterValue
	}
	return prometheus.GaugeValue
}

func parseValue(e Entity) float64 {
	val, err := strconv.ParseFloat(e.State, 64)
	if err == nil {
		return val
	}
	if *verbose {
		log.Debugf("Error parsing state=%s", e.State)
	}
	return -1.0
}

type homeassistantSample struct {
	Id      uint32
	Name    string
	Labels  map[string]string
	Help    string
	Value   float64
	DType   string
	Dstype  string
	Time    float64
	Type    prometheus.ValueType
	Unit    string
	Expires time.Time
}

type homeassistantCollector struct {
	samples map[uint32]*homeassistantSample
	mu      *sync.Mutex
	ch      chan *homeassistantSample
}

func newhomeassistantCollector() *homeassistantCollector {
	c := &homeassistantCollector{
		ch:      make(chan *homeassistantSample, 0),
		mu:      &sync.Mutex{},
		samples: map[uint32]*homeassistantSample{},
	}
	go c.processSamples()
	return c
}

func (c *homeassistantCollector) processSamples() {
	ticker := time.NewTicker(time.Minute).C
	for {
		select {
		case sample := <-c.ch:
			c.mu.Lock()
			c.samples[sample.Id] = sample
			c.mu.Unlock()
		case <-ticker:
			// Garbage collect expired samples.
			now := time.Now()
			c.mu.Lock()
			for k, sample := range c.samples {
				if now.After(sample.Expires) {
					delete(c.samples, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

// Collect implements prometheus.Collector.
func (c homeassistantCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- lastPush

	c.mu.Lock()
	samples := make([]*homeassistantSample, 0, len(c.samples))
	for _, sample := range c.samples {
		samples = append(samples, sample)
	}
	c.mu.Unlock()

	now := time.Now()
	for _, sample := range samples {
		if now.After(sample.Expires) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(sample.Name, sample.Help, []string{}, sample.Labels), sample.Type, sample.Value,
		)
	}
}

// Describe implements prometheus.Collector.
func (c homeassistantCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- lastPush.Desc()
}

func queryServer() []byte {
	log.Info("Polling Home Assistant")
	haClient := http.Client{
		Timeout: time.Second * 2, // Timeout after 2 seconds
	}
	req, err := http.NewRequest(http.MethodGet, *homeAssistantQuery, nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+*homeAssistantAPI)

	res, getErr := haClient.Do(req)
	if getErr != nil {
		log.Fatal(getErr)
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		log.Fatal(readErr)
	}

	return body
}

func task(c homeassistantCollector) {
	body := queryServer()

	entities := []Entity{}
	err := json.Unmarshal(body, &entities)
	if err != nil {
		log.Fatal(err)
	}

	for _, entity := range entities {
		metric, found := mappings[entity.EntityId]
		if found {
			parseTime, err := time.Parse(time.RFC3339, entity.LastUpdated)
			if err != nil {
				panic(err)
			}
			now := time.Now()
			lastPush.Set(float64(now.UnixNano()) / 1e9)
			labels := prometheus.Labels{}
			c.ch <- &homeassistantSample{
				Id:      uint32(parseTime.Unix()),
				Name:    metricName(entity, metric.Name),
				Labels:  labels,
				Help:    metricHelp(entity, metric.Name),
				Value:   parseValue(entity),
				Type:    metricType(entity, metric),
				Expires: now.Add(time.Duration(300) * time.Second * 2),
			}
		}
	}
}

func taskSingle() {

	body := queryServer()

	entities := []Entity{}
	err := json.Unmarshal(body, &entities)
	if err != nil {
		log.Fatal(err)
	}

	for _, entity := range entities {
		metric, found := mappings[entity.EntityId]
		if *verbose {
			log.Debugf("found=%b, entity=%s", found, entity.EntityId)
		}
		if found {
			log.Infof("%s - %f", metricName(entity, metric.Name), parseValue(entity))
		}
	}
}

func main() {
	flag.Parse()

	if *homeAssistantQuery == "" {
		log.Panic("Home Assistant Query not defined")
	}

	if *homeAssistantAPI == "" {
		log.Panic("Home Assistant API not defined")
	}

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	mappingsFile, err := os.Open(*mappingfile)
	if err == nil {
		log.Info("Parsing Mappings file")
		byteValue, _ := ioutil.ReadAll(mappingsFile)
		json.Unmarshal(byteValue, &mappings)
		if *verbose {
			log.Debug(mappings)
		}
		log.Infof("Parsing Mappings file : %d entries", len(mappings))
	} else {
		log.Error(err)
	}
	defer mappingsFile.Close()

	c := newhomeassistantCollector()

	if *homeAssistantTest == true {
		taskSingle()
	} else {
		http.Handle(*metricsPath, promhttp.Handler())
		prometheus.MustRegister(c)

		task(*c)

		s := gocron.NewScheduler()
		s.Every(*homeAssistantPollingRate).Second().Do(task)
		<-s.Start()

		log.Info("Listening on " + *listeningAddress)
		http.ListenAndServe(*listeningAddress, nil)
	}
}
