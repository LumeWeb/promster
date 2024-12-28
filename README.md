# promster

[<img src="https://img.shields.io/docker/pulls/lumeweb/promster"/>](https://hub.docker.com/r/lumeweb/promster)
[<img src="https://img.shields.io/docker/automated/lumeweb/promster"/>](https://hub.docker.com/r/lumeweb/promster)<br/>
[<img src="https://goreportcard.com/badge/github.com/lumeweb/promster"/>](https://goreportcard.com/report/github.com/lumeweb/promster)

Prometheus with dynamic scrape target discovery based on ETCD.

Promster is a process that runs in parallel to Prometheus and operates by changing prometheus.yml and file with lists of hosts/federated Prometheus to be scraped dynamically.

By using record rules and sharding scrape among servers, you can reduce the dimensionality of metrics, keeping then under variability control. Promster manages the list of hosts so that it can be used to configure hierarquical schemes of Prometheus chains for large scale deployments. See an example in diagrams below:


Promster helps you create this kind of Prometheus deployment by helping you create those recording rules, discover scrape targets, discover sibilings, Prometheus instances and then distribute the load across those instances accordingly so that if you scale the number of Prometheus instances the (sharded) load will get distributed over the other instances automatically.

Se an example below:

# Usage

```yml

docker-compose.yml

version: '3.5'

services:

  etcd0:
    image: quay.io/coreos/etcd:v3.2.25
    environment:
      - ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379
      - ETCD_ADVERTISE_CLIENT_URLS=http://etcd0:2379

  generator:
    image: labbsr0x/metrics-generator-tabajara
    environment:
      - COMPONENT_NAME=testserver
      - COMPONENT_VERSION=1.0.0
      - PROMSTER_SCRAPE_ETCD_URL=http://etcd0:2379
      - PROMSTER_ETCD_BASE_PATH=/webservers
    ports:
      - 3000

  promster-level1:
    image: lumeweb/promster
    ports:
      - 9090
    environment:
      - LOG_LEVEL=info
      - SCHEME=http
      - TLS_INSECURE=false

      - SCRAPE_ETCD_URL=http://etcd0:2379
      - SCRAPE_ETCD_PATH=/webservers/generator
      - SCRAPE_INTERVAL=5s
      - SCRAPE_TIMEOUT=3s
      - RETENTION_TIME=30m

      - RECORD_RULE_1_NAME=level1:http_requests_app_total:irate
      - RECORD_RULE_1_EXPR=sum(irate(http_requests_app_total[2m])) without (job,server_name,instance)
      - RECORD_RULE_1_LABELS=site:s1,region:b1

      - EVALUATION_INTERVAL=20s

  promster-level2:
    image: lumeweb/promster
    ports:
      - 9090
    environment:
      - LOG_LEVEL=info

      - SCRAPE_ETCD_URL=http://etcd0:2379
      - SCRAPE_ETCD_PATH=/registry/prom-level1
      - SCRAPE_PATHS=/federate
      - SCRAPE_MATCH_REGEX=level1:.*
      - SCRAPE_INTERVAL=5s
      - SCRAPE_TIMEOUT=3s
      - RETENTION_TIME=30m

      - RECORD_RULE_1_NAME=level2:http_requests_app_total:irate
      - RECORD_RULE_1_EXPR=sum(level1:http_requests_app_total:irate) without (job,instance)

      - RECORD_RULE_2_NAME=total:http_requests_app_total:irate
      - RECORD_RULE_2_EXPR=sum(level1:http_requests_app_total:irate)

      - EVALUATION_INTERVAL=20s

  promster-level3:
    image: lumeweb/promster
    ports:
      - 9090
    environment:
      - LOG_LEVEL=info

      - SCRAPE_ETCD_URL=http://etcd0:2379
      - SCRAPE_ETCD_PATH=/registry/prom-level2
      - SCRAPE_PATHS=/federate
      - SCRAPE_MATCH_REGEX=level2:.*
      - SCRAPE_INTERVAL=5s
      - SCRAPE_TIMEOUT=3s
      - SCRAPE_SHARD_ENABLE=false
      - RETENTION_TIME=30m

      - RECORD_RULE_1_NAME=level3:http_requests_app_total:irate
      - RECORD_RULE_1_EXPR=sum(level2:http_requests_app_total:irate) without (job,instance)
      
      - EVALUATION_INTERVAL=20s

```

* run ```docker-compose up```

* Launch 20 instances of the target server that is being monitored: ```docker-compose scale generator=20```

* Launch 5 instances of the first layer of Prometheus. Each will be responsible for scraping ~4 servers and for aggregating results: ```docker-compose scale promster-level1=5```

* Launch 2 instances of the second layer of Prometheus. Each will be responsible for scraping ~2-3 federated Prometheus servers (from level1) and for aggregating the results: ```docker-compose scale promster-level2=2```

* run ```docker ps``` and get the dynamic port allocated for container promster_level1

* open browser at http://localhost:[dyn port]/targets and see which scrape targets were associated to this node

* Perform final queries at level3. It will have all the aggregated results of all metrics.

# ENV configurations

* LOG_LEVEL 'info'
## Environment Variables

### Core Configuration
* `PROMSTER_LOG_LEVEL` - Log level (debug, info, warning, error). Default: info
* `PROMSTER_SCRAPE_ETCD_URL` - ETCD URLs for service discovery
* `PROMSTER_ETCD_BASE_PATH` - Base ETCD path for service discovery
* `PROMSTER_ETCD_USERNAME` - ETCD username for authentication (optional)
* `PROMSTER_ETCD_PASSWORD` - ETCD password for authentication (optional)
* `PROMSTER_ETCD_TIMEOUT` - ETCD connection timeout. Default: 30s

### Prometheus Configuration
* `PROMSTER_SCRAPE_INTERVAL` - Time between scrapes. Default: 30s
* `PROMSTER_SCRAPE_TIMEOUT` - Timeout for scrape requests. Default: 30s
* `PROMSTER_EVALUATION_INTERVAL` - Time between rule evaluations. Default: 30s
* `PROMSTER_SCHEME` - Target's scheme (http/https). Default: http
* `PROMSTER_TLS_INSECURE` - Disable TLS certificate validation (true/false). Default: false
* `PROMSTER_MONITORING_INTERVAL` - How often to check for service changes. Default: 5s

### Prometheus Authentication
* `PROMETHEUS_ADMIN_USERNAME` - Username for Prometheus admin API (required)
* `PROMETHEUS_ADMIN_PASSWORD` - Password for Prometheus admin API (required)
* `PROMETHEUS_CONFIG_FILE` - Path to Prometheus config file. Default: /prometheus.yml

### Recording Rules
* `RECORD_RULE_[N]_NAME` - Name for recording rule N
* `RECORD_RULE_[N]_EXPR` - PromQL expression for recording rule N
* `RECORD_RULE_[N]_LABELS` - Optional labels for recording rule N (format: key1:value1,key2:value2)

Where [N] is a sequential number (1, 2, 3, etc.) for multiple rules.
