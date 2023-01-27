package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/mcuadros/go-defaults"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	lastPush = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "homeassistant_last_push_timestamp_seconds",
			Help: "Unix timestamp of the last received metrics push in seconds.",
		},
	)
	configuration = &Configuration{}
	config        ExporterConfiguration
)

type ExporterConfig struct {
	ListeningAddress  string `mapstructure:"listeningAddress" default:":9393"`
	MetricsPath       string `mapstructure:"metricsPath" default:"/metrics"`
	HomeAssistantTest bool   `mapstructure:"test" default:"false"`
	ConfigurationFile string `mapstructure:"configurationFile"`
	Verbose           bool   `mapstructure:"verbose" default:"false"`
}

type ExporterConfigHa struct {
	Query       string `mapstructure:"query"`
	ApiKey      string `mapstructure:"api"`
	PollingRate int    `mapstructure:"pollingRate" default:"300"`
}

type ExporterConfiguration struct {
	Config ExporterConfig   `mapstructure:"config"`
	Ha     ExporterConfigHa `mapstructure:"ha"`
}

type MappingEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Configuration struct {
	Mappings map[string]MappingEntry `json:"mappings"`
	Prefix   string                  `json:"prefix"`
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
	result := configuration.Prefix
	result += fmt.Sprintf("%s", name)
	return result
}

func metricHelp(m Entity, name string) string {
	return fmt.Sprintf("homeassistant exporter: Name: '%s'", name)
}

func metricType(e Entity, m MappingEntry) (prometheus.ValueType, error) {
	if m.Type == "counter" || m.Type == "derive" {
		return prometheus.CounterValue, nil
	}
	if m.Type == "gauge" {
		return prometheus.GaugeValue, nil
	}
	return 0, errors.New("Unvalid metric type")
}

func parseValue(e Entity) (float64, error) {
	if e.State != "unavailable" {
		val, err := strconv.ParseFloat(e.State, 64)
		if err == nil {
			return val, err
		}
		if config.Config.Verbose {
			log.Debugf("Error parsing state=%s", e.State)
		}
	}
	return -1.0, errors.New("Unvalid value")
}

type homeassistantSample struct {
	Id      string
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
	samples map[string]*homeassistantSample
	mu      *sync.Mutex
	ch      chan *homeassistantSample
}

func newhomeassistantCollector() *homeassistantCollector {
	c := &homeassistantCollector{
		ch:      make(chan *homeassistantSample, 0),
		mu:      &sync.Mutex{},
		samples: map[string]*homeassistantSample{},
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

func queryServer() ([]byte, error) {
	log.Debug("Polling Home Assistant")
	haClient := http.Client{
		Timeout: time.Second * 2, // Timeout after 2 seconds
	}
	req, err := http.NewRequest(http.MethodGet, config.Ha.Query, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+config.Ha.ApiKey)

	res, getErr := haClient.Do(req)
	if getErr != nil {
		return nil, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		return nil, err
	}

	return body, nil
}

func task(c homeassistantCollector) {

	body, err := queryServer()
	if err != nil {
		log.Fatal(err)
	}

	entities := []Entity{}
	err = json.Unmarshal(body, &entities)
	if err != nil {
		log.Fatal(err)
	}

	for _, entity := range entities {
		metric, found := configuration.Mappings[entity.EntityId]
		if found {
			value, err := parseValue(entity)
			if err == nil {
				now := time.Now()
				lastPush.Set(float64(now.UnixNano()) / 1e9)
				metricType, err := metricType(entity, metric)
				if err == nil {
					labels := prometheus.Labels{}
					c.ch <- &homeassistantSample{
						Id:      metric.Name,
						Name:    metricName(entity, metric.Name),
						Labels:  labels,
						Help:    metricHelp(entity, metric.Name),
						Value:   value,
						Type:    metricType,
						Expires: now.Add(time.Duration(config.Ha.PollingRate) * time.Second * 2),
					}
				} else {
					log.Warnf("Wrong metric type for %s: %s", metric.Name, metric.Type)
				}
			}
		}
	}
}

func taskSingle() {

	body, err := queryServer()
	if err != nil {
		log.Fatal(err)
	}

	entities := []Entity{}
	err = json.Unmarshal(body, &entities)
	if err != nil {
		log.Fatal(err)
	}

	for _, entity := range entities {
		metric, found := configuration.Mappings[entity.EntityId]
		if config.Config.Verbose {
			log.Infof("found=%t, entity=%s", found, entity.EntityId)
		}
		if found {
			value, err := parseValue(entity)
			if err == nil {
				log.Infof("%s - %f", metricName(entity, metric.Name), value)
			} else {
				log.Warnf("Wrong value for metric %s: %s", metric.Name, entity.State)
			}
		}
	}
}

func LoadConfig(path string) (err error) {
	viper.AddConfigPath(path)
	viper.SetConfigName("homeassistant_exporter")
	viper.SetConfigType("json")

	viper.AutomaticEnv()

	err = viper.ReadInConfig()
	if err != nil {
		return
	}

	defaults.SetDefaults(&config)
	err = viper.Unmarshal(&config)

	return
}

func startExporter() {

	if config.Config.Verbose {
		log.SetLevel(log.DebugLevel)
	}

	configurationFile, err := os.Open(config.Config.ConfigurationFile)
	if err == nil {
		log.Info("Parsing Configuration file")
		byteValue, _ := ioutil.ReadAll(configurationFile)
		json.Unmarshal(byteValue, &configuration)
		if config.Config.Verbose {
			log.Debug(configuration)
		}
		log.Infof("Parsing Configuration file: %d entries", len(configuration.Mappings))
		defer configurationFile.Close()
	} else {
		log.Fatalf("Failed to open configuration file: %s", config.Config.ConfigurationFile)
	}

	c := newhomeassistantCollector()

	if config.Config.HomeAssistantTest == true {
		taskSingle()
	} else {
		prometheus.MustRegister(c)

		s := gocron.NewScheduler(time.Now().Location())
		s.Every(config.Ha.PollingRate).Second().Do(func() { task(*c) })
		s.StartAsync()

		log.Info("Listening on " + config.Config.ListeningAddress)
		http.Handle(config.Config.MetricsPath, promhttp.Handler())
		http.ListenAndServe(config.Config.ListeningAddress, nil)
	}

}

func main() {
	viper.SetEnvPrefix("HA_EXPORTER")

	err := LoadConfig(".")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	startExporter()
}
