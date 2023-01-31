# Home Assistant Exporter

# Presentation
An exporter for [Home Assistant](https://www.home-assistant.io/). It polls metrics
in JSON format via HTTP GET, transforms them and exposes them for consumption by [Prometheus](https://www.prometheus.io/).

# Purpose
Home Assistant allow prometheus export. However, the metric names and labels are fixed, cannot be filtered and if an entity is renamed, the metric is renamed in prometheus and data are lost without changing the dashboard.

# Configuration
This exporter uses a configuration file containing mapping entries to defines which entity is exported, if it's a gauge or a counter.
The metric prefix can be configured as well.

## Example
```
{
    "prefix": "homeassistant_",
    "mappings": {
        "sensor.ewelink_th01_temperature": {
        "name": "temperature_outside",
        "type": "gauge"
    }
}
```

# Usage
* Build the container from the source:
```
docker build -t homeassistant_exporter
```
* Start the container:
```
docker run -d -p 9103:9103 --name=homeassistant_exporter --network bouchex --restart=always -v homeassistant_exporter:/homeassistant_exporter_data homeassistant_exporter:latest /homeassistant_exporter --ha.api='<ap.key>' --ha.rate=300 --ha.query=http://<home assistant host>:<home assistant port>/api/states
```
