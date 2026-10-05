SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:
.ONESHELL:
.NOTPARALLEL: up ready reset stop start traffic scenario-read scenario-replan \
	siem-on siem-off siem-rule siem-verify

PROJECT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

GO ?= go
GOFLAGS ?=
DOCKER ?= docker
DOCKER_FLAGS ?=
MINIKUBE ?= minikube
MINIKUBE_FLAGS ?=
KUBECTL ?= kubectl
KUBECTL_FLAGS ?=
HELM ?= helm
HELM_FLAGS ?=
CURL ?= curl
CURL_FLAGS ?=
JQ ?= jq
JQ_FLAGS ?=
SEQ ?= seq

MINIKUBE_PROFILE ?= trace-lab
MINIKUBE_CPUS ?= 6
MINIKUBE_MEMORY ?= 12288
MINIKUBE_DISK ?= 30g
IMAGE ?= trace-lab:dev
IMAGE_TAG ?= $(IMAGE)

OTEL_OPERATOR_VERSION ?= 0.123.0
TEMPO_VERSION ?= 3.0.0
TEMPO_IMAGE ?= grafana/tempo:3.0.3
CNPG_OPERATOR_VERSION ?= 0.29.0
CNPG_CLUSTER_VERSION ?= 0.8.1
PROMETHEUS_VERSION ?= 29.33.0
LOKI_VERSION ?= 18.13.5
OPENSEARCH_VERSION ?= 3.8.0
OPENSEARCH_IMAGE ?= opensearchproject/opensearch:$(OPENSEARCH_VERSION)
OPENSEARCH_DASHBOARDS_IMAGE ?= opensearchproject/opensearch-dashboards:$(OPENSEARCH_VERSION)
DATAPREPPER_VERSION ?= 0.3.1
DATAPREPPER_IMAGE ?= opensearchproject/data-prepper:2.8.0
OPENSEARCH_NAMESPACE ?= opensearch
INTERNAL_KEY ?=
DATABASE_USER ?= dispatch
DATABASE_PASSWORD ?=
OPENSEARCH_API_LOCAL_PORT ?= 19200
SIEM_MONITOR_NAME ?= trace-lab-unexpected-edge
SIEM_EDGE_ANNOTATION ?= trace-lab.io/allowed-edges

NAMESPACES ?= dispatch-core config otel tempo prometheus loki cnpg-system

.PHONY: help check up ready reset stop start status ports traffic \
	scenario-read scenario-replan siem-on siem-rule siem-off siem-ports \
	siem-verify _check _cluster _addons _image

help: ## Show the available commands.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: ## Run formatting and static analysis.
	test -z "$$($(GO)fmt -d main.go)"
	$(GO) vet $(GOFLAGS) ./...

_check:
	for command in $(GO) $(DOCKER) $(HELM) $(KUBECTL) $(MINIKUBE) $(CURL) $(JQ) $(SEQ) openssl; do
		command -v "$$command" >/dev/null || { echo "Required command not found: $$command" >&2; exit 1; }
	done
	$(DOCKER) info >/dev/null

_cluster: _check
	@if [ "$$($(MINIKUBE) $(MINIKUBE_FLAGS) status --profile $(MINIKUBE_PROFILE) --format='{{.Host}}' 2>/dev/null || true)" = 'Running' ]; then \
		echo 'Minikube profile $(MINIKUBE_PROFILE) is already running'; \
	else \
		$(MINIKUBE) $(MINIKUBE_FLAGS) start --profile $(MINIKUBE_PROFILE) --driver=docker \
			--cpus=$(MINIKUBE_CPUS) --memory=$(MINIKUBE_MEMORY) --disk-size=$(MINIKUBE_DISK); \
	fi

_addons: _cluster
	@for namespace in $(NAMESPACES); do \
		$(KUBECTL) $(KUBECTL_FLAGS) create namespace "$${namespace}" --dry-run=client -o yaml | $(KUBECTL) $(KUBECTL_FLAGS) apply -f -; \
	done
	$(HELM) $(HELM_FLAGS) upgrade --install cnpg oci://ghcr.io/cloudnative-pg/charts/cloudnative-pg \
		--version $(CNPG_OPERATOR_VERSION) \
		--namespace cnpg-system --create-namespace
	$(HELM) $(HELM_FLAGS) upgrade --install otel-operator oci://ghcr.io/open-telemetry/opentelemetry-helm-charts/opentelemetry-operator \
		--version $(OTEL_OPERATOR_VERSION) \
		--namespace otel --create-namespace \
		--set manager.autoInstrumentation.go.enabled=true \
		--set manager.autoInstrumentationImage.go.repository=ghcr.io/open-telemetry/opentelemetry-go-instrumentation/autoinstrumentation-go \
		--set manager.autoInstrumentationImage.go.tag=v0.24.0 \
		--set admissionWebhooks.certManager.enabled=false \
		--set admissionWebhooks.autoGenerateCert.enabled=true \
		--set admissionWebhooks.autoGenerateCert.recreate=false
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status deployment/otel-operator-opentelemetry-operator --timeout=180s
	@if ! $(DOCKER) $(DOCKER_FLAGS) image inspect $(TEMPO_IMAGE) >/dev/null 2>&1; then \
		$(DOCKER) $(DOCKER_FLAGS) pull --quiet $(TEMPO_IMAGE); \
	fi
	$(MINIKUBE) $(MINIKUBE_FLAGS) image load --profile $(MINIKUBE_PROFILE) $(TEMPO_IMAGE)
	$(HELM) $(HELM_FLAGS) upgrade --install tempo oci://ghcr.io/grafana-community/helm-charts/tempo \
		--version $(TEMPO_VERSION) \
		--namespace tempo --create-namespace \
		--set replicas=1 \
		--set persistence.enabled=false \
		--set tempo.reportingEnabled=false \
		--set tempo.retention=1h \
		--set tempo.liveStore.max_trace_idle=1s \
		--set tempo.liveStore.flush_check_period=1s \
		--set tempo.liveStore.max_block_duration=5s \
		--set tempo.liveStore.complete_block_timeout=2m \
		--set tempo.queryFrontend.query_end_cutoff=0s \
		--set tempo.queryFrontend.search.query_backend_after=30s \
		--set tempo.storage.trace.blocklist_poll=1s \
		--set service.type=ClusterIP
	$(HELM) $(HELM_FLAGS) upgrade --install prometheus oci://ghcr.io/prometheus-community/charts/prometheus \
		--version $(PROMETHEUS_VERSION) \
		--namespace prometheus --create-namespace \
		--set alertmanager.enabled=false \
		--set kube-state-metrics.enabled=false \
		--set prometheus-node-exporter.enabled=false \
		--set prometheus-pushgateway.enabled=false \
		--set scrapeConfigs.prometheus.enabled=false \
		--set scrapeConfigs.kubernetes-api-servers.enabled=false \
		--set scrapeConfigs.kubernetes-nodes.enabled=false \
		--set scrapeConfigs.kubernetes-nodes-cadvisor.enabled=false \
		--set scrapeConfigs.kubernetes-service-endpoints.enabled=false \
		--set scrapeConfigs.kubernetes-service-endpoints-slow.enabled=false \
		--set scrapeConfigs.prometheus-pushgateway.enabled=false \
		--set scrapeConfigs.kubernetes-services.enabled=false \
		--set scrapeConfigs.kubernetes-pods.enabled=false \
		--set scrapeConfigs.kubernetes-pods-slow.enabled=false \
		--set server.persistentVolume.enabled=false \
		--set 'server.extraArgs.web\.enable-remote-write-receiver=null'
	$(KUBECTL) $(KUBECTL_FLAGS) -n prometheus rollout restart deployment/prometheus-server
	$(HELM) $(HELM_FLAGS) upgrade --install loki oci://ghcr.io/grafana-community/helm-charts/loki \
		--version $(LOKI_VERSION) \
		--namespace loki --create-namespace \
		--set deploymentMode=Monolithic \
		--set loki.auth_enabled=false \
		--set loki.commonConfig.replication_factor=1 \
		--set loki.storage.type=filesystem \
		--set gateway.enabled=false \
		--set singleBinary.replicas=1 \
		--set backend.replicas=0 \
		--set read.replicas=0 \
		--set write.replicas=0 \
		--set chunksCache.enabled=false \
		--set resultsCache.enabled=false \
		--set singleBinary.persistence.enabled=false \
		--set singleBinary.sidecar=false \
		--set lokiCanary.enabled=false \
		--set test.enabled=false \
		--set loki.schemaConfig.configs[0].from=2024-04-01 \
		--set loki.schemaConfig.configs[0].store=tsdb \
		--set loki.schemaConfig.configs[0].object_store=filesystem \
		--set loki.schemaConfig.configs[0].schema=v13 \
		--set loki.schemaConfig.configs[0].index.prefix=index_ \
		--set loki.schemaConfig.configs[0].index.period=24h
	$(KUBECTL) $(KUBECTL_FLAGS) -n tempo rollout status statefulset/tempo --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) -n prometheus rollout status deployment/prometheus-server --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) -n loki rollout status statefulset/loki --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) apply -f telemetry.yaml
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status deployment/otel-gateway-collector --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status daemonset/otel-logs-collector --timeout=180s
_image: _cluster
	$(DOCKER) $(DOCKER_FLAGS) build --label trace-lab.build="$$(date +%s%N)" --tag $(IMAGE_TAG) $(PROJECT_DIR)
	$(MINIKUBE) $(MINIKUBE_FLAGS) image load --profile $(MINIKUBE_PROFILE) $(IMAGE_TAG)

up: _check _cluster _image _addons ## Prepare the local platform without deploying applications.
	@if ! $(KUBECTL) $(KUBECTL_FLAGS) -n config get secret dispatch-internal-auth >/dev/null 2>&1; then \
		internal_key='$(INTERNAL_KEY)'; \
		if [ -z "$${internal_key}" ]; then internal_key=$$(openssl rand -hex 24); fi; \
		$(KUBECTL) $(KUBECTL_FLAGS) -n config create secret generic dispatch-internal-auth \
			--from-literal=INTERNAL_KEY="$${internal_key}"; \
	fi
	@if ! $(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core get secret dispatch-db-app >/dev/null 2>&1; then \
		db_password='$(DATABASE_PASSWORD)'; \
		if [ -z "$${db_password}" ]; then db_password=$$(openssl rand -hex 24); fi; \
		database_url="postgres://$(DATABASE_USER):$${db_password}@dispatch-db-rw.dispatch-core.svc.cluster.local:5432/dispatch?sslmode=disable"; \
		$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core create secret generic dispatch-db-app \
			--type=kubernetes.io/basic-auth \
			--from-literal=username=$(DATABASE_USER) \
			--from-literal=password="$${db_password}" \
			--from-literal=DATABASE_URL="$${database_url}"; \
	fi
	$(HELM) $(HELM_FLAGS) upgrade --install dispatch-db oci://ghcr.io/cloudnative-pg/charts/cluster \
		--version $(CNPG_CLUSTER_VERSION) \
		--namespace dispatch-core --create-namespace \
		--set fullnameOverride=dispatch-db \
		--set type=postgresql \
		--set-string version.postgresql=16 \
		--set cluster.initdb.database=dispatch \
		--set cluster.initdb.owner=dispatch \
		--set cluster.initdb.secret.name=dispatch-db-app \
		--set cluster.instances=1 \
		--set cluster.storage.size=1Gi \
		--set cluster.affinity.topologyKey=kubernetes.io/hostname \
		--set cluster.enablePDB=false
	$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core wait --for=condition=Ready cluster/dispatch-db --timeout=300s
	@echo
	@echo 'Platform is ready.'
	@echo 'Deploy application workloads from the README with kubectl.'

ready: _check ## Verify that the PoC workloads are ready.
	$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core wait --for=condition=ready pod -l app=tracking-api --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) -n config wait --for=condition=ready pod -l app=config-server --timeout=180s
	for deployment in shipment-api routing-api tracking-api notification-api; do
		$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core rollout status deployment/"$$deployment" --timeout=180s
	done
	$(KUBECTL) $(KUBECTL_FLAGS) -n config rollout status deployment/config-server --timeout=180s
	echo 'Ready.'

stop: _check ## Stop the Minikube profile and keep its data.
	$(MINIKUBE) $(MINIKUBE_FLAGS) stop --profile $(MINIKUBE_PROFILE)

start: _check ## Start an existing Minikube profile.
	$(MINIKUBE) $(MINIKUBE_FLAGS) start --profile $(MINIKUBE_PROFILE)

status: _check ## Show Pods and Services in the PoC.
	$(KUBECTL) $(KUBECTL_FLAGS) get pods -A
	$(KUBECTL) $(KUBECTL_FLAGS) get svc -n dispatch-core
	$(KUBECTL) $(KUBECTL_FLAGS) get svc -n config
	$(KUBECTL) $(KUBECTL_FLAGS) get svc -n tempo

ports: ## Print the core port forwards.
	@echo 'Tempo:      kubectl -n tempo port-forward svc/tempo 3200:3200'
	@echo 'Prometheus: kubectl -n prometheus port-forward svc/prometheus-server 9090:80'
	@echo 'Loki:       kubectl -n loki port-forward svc/loki 3100:3100'
	@echo 'Tracking:   kubectl -n dispatch-core port-forward svc/tracking-api 18081:8080'
	@echo 'Shipment:   kubectl -n dispatch-core port-forward svc/shipment-api 18080:8080'

traffic: _check ## Generate normal application traffic.
	$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core port-forward svc/tracking-api 18081:8080 >/tmp/trace-lab-tracking-port-forward.log 2>&1 & tracking_pid=$$!; \
	$(KUBECTL) $(KUBECTL_FLAGS) -n dispatch-core port-forward svc/shipment-api 18080:8080 >/tmp/trace-lab-shipment-port-forward.log 2>&1 & shipment_pid=$$!; \
	trap 'kill "$${tracking_pid}" "$${shipment_pid}" 2>/dev/null || true' EXIT; \
	sleep 2; \
	echo 'normal traffic is running'; \
	while true; do \
		$(CURL) $(CURL_FLAGS) --fail --silent http://127.0.0.1:18081/v1/track/SHP-9182 >/dev/null; \
		$(CURL) $(CURL_FLAGS) --fail --silent --request POST http://127.0.0.1:18080/v1/shipments/plan >/dev/null; \
		sleep 2; \
	done

scenario-read: _check ## Run the compromised read scenario.
	pod=$$($(KUBECTL) $(KUBECTL_FLAGS) -n config get pods -l app=config-server --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}'); \
	$(KUBECTL) $(KUBECTL_FLAGS) -n config exec "$$pod" -c config-server -- /app/trace-lab scenario-read

scenario-replan: _check ## Run the compromised replan scenario.
	pod=$$($(KUBECTL) $(KUBECTL_FLAGS) -n config get pods -l app=config-server --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}'); \
	$(KUBECTL) $(KUBECTL_FLAGS) -n config exec "$$pod" -c config-server -- /app/trace-lab scenario-replan

siem-on: _check _cluster ## Enable the optional OpenSearch telemetry route.
	$(HELM) $(HELM_FLAGS) repo add opensearch https://opensearch-project.github.io/helm-charts/ >/dev/null 2>&1 || true
	$(HELM) $(HELM_FLAGS) repo update opensearch
	@if ! $(DOCKER) $(DOCKER_FLAGS) image inspect $(DATAPREPPER_IMAGE) >/dev/null 2>&1; then \
		$(DOCKER) $(DOCKER_FLAGS) pull --quiet $(DATAPREPPER_IMAGE); \
	fi
	@if ! $(DOCKER) $(DOCKER_FLAGS) image inspect $(OPENSEARCH_IMAGE) >/dev/null 2>&1; then \
		$(DOCKER) $(DOCKER_FLAGS) pull --quiet $(OPENSEARCH_IMAGE); \
	fi
	@if ! $(DOCKER) $(DOCKER_FLAGS) image inspect $(OPENSEARCH_DASHBOARDS_IMAGE) >/dev/null 2>&1; then \
		$(DOCKER) $(DOCKER_FLAGS) pull --quiet $(OPENSEARCH_DASHBOARDS_IMAGE); \
	fi
	$(MINIKUBE) $(MINIKUBE_FLAGS) image load --profile $(MINIKUBE_PROFILE) $(DATAPREPPER_IMAGE)
	$(MINIKUBE) $(MINIKUBE_FLAGS) image load --profile $(MINIKUBE_PROFILE) $(OPENSEARCH_IMAGE)
	$(MINIKUBE) $(MINIKUBE_FLAGS) image load --profile $(MINIKUBE_PROFILE) $(OPENSEARCH_DASHBOARDS_IMAGE)
	$(KUBECTL) $(KUBECTL_FLAGS) create namespace $(OPENSEARCH_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) $(KUBECTL_FLAGS) apply -f -
	$(HELM) $(HELM_FLAGS) upgrade --install opensearch opensearch/opensearch \
		--reset-values \
		--version $(OPENSEARCH_VERSION) \
		--namespace $(OPENSEARCH_NAMESPACE) \
		--set singleNode=true \
		--set replicas=1 \
		--set persistence.enabled=false \
		--set protocol=http \
		--set image.pullPolicy=IfNotPresent \
		--set-string opensearchJavaOpts='-Xms512M -Xmx512M' \
		--set resources.requests.cpu=250m \
		--set resources.requests.memory=768Mi \
		--set resources.limits.cpu=1 \
		--set resources.limits.memory=1Gi \
		--set-string extraEnvs[0].name=DISABLE_SECURITY_PLUGIN \
		--set-string extraEnvs[0].value=true
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) rollout status statefulset/opensearch-cluster-master --timeout=300s
	@for attempt in $$($(SEQ) 1 60); do \
		health=$$($(KUBECTL) $(KUBECTL_FLAGS) exec -n $(OPENSEARCH_NAMESPACE) statefulset/opensearch-cluster-master -- \
			sh -c "$(CURL) $(CURL_FLAGS) --silent --show-error 'http://127.0.0.1:9200/_cluster/health?wait_for_status=yellow'" 2>/dev/null || true); \
		if printf '%s' "$${health}" | $(JQ) $(JQ_FLAGS) -e '.status == "yellow" or .status == "green"' >/dev/null 2>&1; then \
			printf '%s\n' 'OpenSearch API is ready'; \
			break; \
		fi; \
		if [ "$${attempt}" -eq 60 ]; then \
			printf '%s\n' 'OpenSearch API did not become ready' >&2; \
			exit 1; \
		fi; \
		sleep 2; \
	done
	$(HELM) $(HELM_FLAGS) upgrade --install data-prepper opensearch/data-prepper \
		--reset-values \
		--version $(DATAPREPPER_VERSION) \
		--namespace $(OPENSEARCH_NAMESPACE) \
		--set replicas=1 \
		--set pipelineConfig.enabled=true \
		--set pipelineConfig.config.version=2 \
		--set pipelineConfig.config.otel-logs-pipeline.workers=1 \
		--set pipelineConfig.config.otel-logs-pipeline.delay=100 \
		--set pipelineConfig.config.otel-logs-pipeline.source.otel_logs_source.port=21892 \
		--set pipelineConfig.config.otel-logs-pipeline.source.otel_logs_source.ssl=false \
		--set pipelineConfig.config.otel-logs-pipeline.buffer.bounded_blocking.buffer_size=128 \
		--set pipelineConfig.config.otel-logs-pipeline.buffer.bounded_blocking.batch_size=16 \
		--set "pipelineConfig.config.otel-logs-pipeline.sink[0].opensearch.hosts[0]=http://opensearch-cluster-master.$(OPENSEARCH_NAMESPACE).svc.cluster.local:9200" \
		--set pipelineConfig.config.otel-logs-pipeline.sink[0].opensearch.index_type=custom \
		--set 'pipelineConfig.config.otel-logs-pipeline.sink[0].opensearch.index=logs-otel-v1-%{yyyy.MM.dd}' \
		--set pipelineConfig.config.otel-logs-pipeline.sink[0].opensearch.flush_timeout=2000 \
		--set pipelineConfig.config.otel-traces-pipeline.workers=1 \
		--set pipelineConfig.config.otel-traces-pipeline.delay=100 \
		--set pipelineConfig.config.otel-traces-pipeline.source.otel_trace_source.port=21890 \
		--set pipelineConfig.config.otel-traces-pipeline.source.otel_trace_source.ssl=false \
		--set pipelineConfig.config.otel-traces-pipeline.buffer.bounded_blocking.buffer_size=128 \
		--set pipelineConfig.config.otel-traces-pipeline.buffer.bounded_blocking.batch_size=16 \
		--set pipelineConfig.config.otel-traces-pipeline.sink[0].pipeline.name=traces-raw-pipeline \
		--set pipelineConfig.config.traces-raw-pipeline.workers=1 \
		--set pipelineConfig.config.traces-raw-pipeline.delay=100 \
		--set pipelineConfig.config.traces-raw-pipeline.source.pipeline.name=otel-traces-pipeline \
		--set pipelineConfig.config.traces-raw-pipeline.buffer.bounded_blocking.buffer_size=128 \
		--set pipelineConfig.config.traces-raw-pipeline.buffer.bounded_blocking.batch_size=16 \
		--set pipelineConfig.config.traces-raw-pipeline.processor[0].otel_traces.trace_flush_interval=5 \
		--set "pipelineConfig.config.traces-raw-pipeline.sink[0].opensearch.hosts[0]=http://opensearch-cluster-master.$(OPENSEARCH_NAMESPACE).svc.cluster.local:9200" \
		--set pipelineConfig.config.traces-raw-pipeline.sink[0].opensearch.index_type=trace-analytics-raw \
		--set pipelineConfig.config.traces-raw-pipeline.sink[0].opensearch.flush_timeout=2000 \
		--set image.tag=2.8.0 \
		--set 'extraEnvs[0].name=JAVA_OPTS' \
		--set 'extraEnvs[0].value=-Xms256m -Xmx256m' \
		--set ports[0].name=otel-traces \
		--set ports[0].port=21890 \
		--set ports[1].name=otel-logs \
		--set ports[1].port=21892 \
		--set serviceAccount.create=false \
		--set resources.requests.cpu=100m \
		--set resources.requests.memory=384Mi \
		--set resources.limits.cpu=500m \
		--set resources.limits.memory=768Mi
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) patch deployment data-prepper --type=json --patch="$$(printf '%s' '[{"op":"add","path":"/spec/template/spec/containers/0/startupProbe","value":{"httpGet":{"path":"/list","port":"server"},"periodSeconds":5,"timeoutSeconds":2,"failureThreshold":24}}]')"
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) rollout status deployment/data-prepper --timeout=300s
	$(HELM) $(HELM_FLAGS) upgrade --install opensearch-dashboards opensearch/opensearch-dashboards \
		--reset-values \
		--version $(OPENSEARCH_VERSION) \
		--namespace $(OPENSEARCH_NAMESPACE) \
		--set opensearchHosts=http://opensearch-cluster-master.$(OPENSEARCH_NAMESPACE).svc.cluster.local:9200 \
		--set replicaCount=1 \
		--set image.pullPolicy=IfNotPresent \
		--set resources.requests.cpu=50m \
		--set resources.requests.memory=256Mi \
		--set resources.limits.cpu=500m \
		--set resources.limits.memory=512Mi \
		--set extraEnvs[0].name=NODE_OPTIONS \
		--set extraEnvs[0].value=--max-old-space-size=256 \
		--set-string extraEnvs[1].name=SERVER_SSL_ENABLED \
		--set-string extraEnvs[1].value=false \
		--set-string extraEnvs[2].name=DISABLE_SECURITY_DASHBOARDS_PLUGIN \
		--set-string extraEnvs[2].value=true
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) rollout status deployment/opensearch-dashboards --timeout=300s
	$(KUBECTL) $(KUBECTL_FLAGS) apply -f telemetry.yaml
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel patch opentelemetrycollector otel-gateway --type=json --patch='[{"op":"replace","path":"/spec/config/processors/resource~1siem/attributes/0/value","value":"true"}]'
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel patch opentelemetrycollector otel-logs --type=json --patch='[{"op":"replace","path":"/spec/config/processors/resource~1siem/attributes/0/value","value":"true"}]'
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status deployment/otel-gateway-collector --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status daemonset/otel-logs-collector --timeout=180s
	@echo 'telemetry route enabled'

siem-off: _check ## Disable the optional OpenSearch telemetry route.
	@if $(KUBECTL) $(KUBECTL_FLAGS) -n otel get opentelemetrycollector otel-gateway >/dev/null 2>&1; then \
		$(KUBECTL) $(KUBECTL_FLAGS) -n otel patch opentelemetrycollector otel-gateway --type=json --patch='[{"op":"replace","path":"/spec/config/processors/resource~1siem/attributes/0/value","value":"false"}]'; \
	fi
	@if $(KUBECTL) $(KUBECTL_FLAGS) -n otel get opentelemetrycollector otel-logs >/dev/null 2>&1; then \
		$(KUBECTL) $(KUBECTL_FLAGS) -n otel patch opentelemetrycollector otel-logs --type=json --patch='[{"op":"replace","path":"/spec/config/processors/resource~1siem/attributes/0/value","value":"false"}]'; \
	fi
	@if $(KUBECTL) $(KUBECTL_FLAGS) -n otel get deployment/otel-gateway-collector >/dev/null 2>&1; then \
		$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status deployment/otel-gateway-collector --timeout=180s; \
	fi
	@if $(KUBECTL) $(KUBECTL_FLAGS) -n otel get daemonset/otel-logs-collector >/dev/null 2>&1; then \
		$(KUBECTL) $(KUBECTL_FLAGS) -n otel rollout status daemonset/otel-logs-collector --timeout=180s; \
	fi
	@echo 'telemetry route disabled'

siem-rule: _check _cluster ## Create the unexpected-edge OpenSearch monitor.
	@if ! $(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) get service opensearch-cluster-master >/dev/null 2>&1; then \
		echo 'OpenSearch is not installed; run make siem-on first' >&2; \
		exit 1; \
	fi
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) port-forward svc/opensearch-cluster-master $(OPENSEARCH_API_LOCAL_PORT):9200 >/tmp/trace-lab-opensearch-rule-port-forward.log 2>&1 & pid=$$!; \
	trap 'kill "$${pid}" 2>/dev/null || true' EXIT; \
	sleep 3; \
	policy=$$($(KUBECTL) $(KUBECTL_FLAGS) get deployments -A -o json | $(JQ) $(JQ_FLAGS) -c --arg annotation '$(SIEM_EDGE_ANNOTATION)' '[.items[] | . as $$deployment | ($$deployment.metadata.annotations[$$annotation] // "[]") | fromjson[] | {caller: $$deployment.metadata.name, target, method, path}]'); \
	edges=$$($(JQ) $(JQ_FLAGS) 'length' <<<"$${policy}"); \
	if [ "$${edges}" -eq 0 ]; then \
		echo 'no allowed edges found in workload annotations' >&2; \
		exit 1; \
	fi; \
	must_not=$$($(JQ) $(JQ_FLAGS) -c '[.[] | {bool: {filter: [\
		{term: {serviceName: .caller}},\
		{term: {"span.attributes.server@address": .target}},\
		{term: {"span.attributes.http@request@method": .method}},\
		(if (.path | contains("*")) then {wildcard: {"span.attributes.url@path": .path}} else {term: {"span.attributes.url@path": .path}} end)\
	]}}]' <<<"$${policy}"); \
	monitor=$$($(JQ) $(JQ_FLAGS) -n --arg name '$(SIEM_MONITOR_NAME)' --argjson must_not "$${must_not}" '{\
		type: "monitor",\
		name: $$name,\
		monitor_type: "query_level_monitor",\
		enabled: true,\
		schedule: {period: {interval: 1, unit: "MINUTES"}},\
		inputs: [{search: {indices: ["otel-v1-apm-span-*"], query: {\
			size: 20,\
			_source: ["traceId", "serviceName", "kind", "startTime", "span.attributes.server@address", "span.attributes.http@request@method", "span.attributes.url@path", "span.attributes.url@full", "span.attributes.http@response@status_code"],\
			query: {bool: {\
				filter: [\
					{term: {kind: "SPAN_KIND_CLIENT"}},\
					{exists: {field: "span.attributes.server@address"}},\
					{exists: {field: "span.attributes.http@request@method"}},\
					{exists: {field: "span.attributes.url@path"}},\
					{range: {startTime: {from: "{{period_end}}||-2m", to: "{{period_end}}", include_lower: true, include_upper: true}}}\
				],\
				must_not: $$must_not\
			}},\
			aggregations: {unexpected_edges: {composite: {size: 100, sources: [\
				{caller: {terms: {field: "serviceName"}}},\
				{target: {terms: {field: "span.attributes.server@address"}}},\
				{method: {terms: {field: "span.attributes.http@request@method"}}},\
				{path: {terms: {field: "span.attributes.url@path"}}}\
			]}}}\
		}}}],\
		triggers: [{\
			name: "unexpected-edge",\
			severity: "1",\
			condition: {script: {source: "ctx.results[0].hits.total.value > 0", lang: "painless"}},\
			actions: []\
		}]\
	}'); \
	search_body=$$($(JQ) $(JQ_FLAGS) -n --arg name '$(SIEM_MONITOR_NAME)' '{query: {match: {"monitor.name": $$name}}}'); \
	monitor_id=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error --request GET --header 'Content-Type: application/json' --data-raw "$${search_body}" "http://127.0.0.1:$(OPENSEARCH_API_LOCAL_PORT)/_plugins/_alerting/monitors/_search" | $(JQ) $(JQ_FLAGS) -r '.hits.hits[0]._id // empty'); \
	if [ -n "$${monitor_id}" ]; then \
		$(CURL) $(CURL_FLAGS) --fail --silent --show-error --request DELETE "http://127.0.0.1:$(OPENSEARCH_API_LOCAL_PORT)/_plugins/_alerting/monitors/$${monitor_id}" >/dev/null; \
	fi; \
	result=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error --request POST --header 'Content-Type: application/json' --data-raw "$${monitor}" "http://127.0.0.1:$(OPENSEARCH_API_LOCAL_PORT)/_plugins/_alerting/monitors"); \
	printf 'trusted edges      %s\n' "$${edges}"; \
	printf 'OpenSearch monitor %s\n' "$$(printf '%s' "$${result}" | $(JQ) $(JQ_FLAGS) -r '._id // .monitor_id // .monitor_name // .name // "created"')"; \
	printf 'detection scope    caller, target, method and path\n'

siem-ports: ## Print the optional OpenSearch port forwards.
	@echo 'OpenSearch API: kubectl -n $(OPENSEARCH_NAMESPACE) port-forward svc/opensearch-cluster-master 9200:9200'
	@echo 'Dashboards:     kubectl -n $(OPENSEARCH_NAMESPACE) port-forward svc/opensearch-dashboards 5601:5601'
	@echo 'Data Prepper traces: kubectl -n $(OPENSEARCH_NAMESPACE) port-forward svc/data-prepper 21890:21890'
	@echo 'Data Prepper logs:   kubectl -n $(OPENSEARCH_NAMESPACE) port-forward svc/data-prepper 21892:21892'

siem-verify: siem-on siem-rule
	$(KUBECTL) $(KUBECTL_FLAGS) wait --for=condition=ready pod -n dispatch-core -l app=tracking-api --timeout=180s
	$(KUBECTL) $(KUBECTL_FLAGS) wait --for=condition=ready pod -n config -l app=config-server --timeout=180s
	scenario_start=$$(date -u +%Y-%m-%dT%H:%M:%SZ); \
	$(MAKE) scenario-read; \
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(OPENSEARCH_NAMESPACE) port-forward svc/opensearch-cluster-master 19200:9200 >/tmp/trace-lab-opensearch-port-forward.log 2>&1 & pid=$$!; \
	trap 'kill "$${pid}" 2>/dev/null || true' EXIT; \
	sleep 3; \
	for attempt in $$($(SEQ) 1 30); do \
		span_docs=$$($(CURL) $(CURL_FLAGS) --silent 'http://127.0.0.1:19200/otel-v1-apm-span-*/_count' | $(JQ) $(JQ_FLAGS) -r '.count // 0'); \
		log_docs=$$($(CURL) $(CURL_FLAGS) --silent 'http://127.0.0.1:19200/logs-otel-v1-*/_count' | $(JQ) $(JQ_FLAGS) -r '.count // 0'); \
		edge_docs=$$($(CURL) $(CURL_FLAGS) --silent --get --data-urlencode "q=serviceName:\"config-server\" AND kind:\"SPAN_KIND_CLIENT\" AND span.attributes.server@address:\"tracking-api.dispatch-core.svc.cluster.local\" AND span.attributes.url@path:\"/internal/fleet/positions\" AND startTime:[\"$${scenario_start}\" TO now]" 'http://127.0.0.1:19200/otel-v1-apm-span-*/_count' | $(JQ) $(JQ_FLAGS) -r '.count // 0'); \
		tracking_status=$$($(CURL) $(CURL_FLAGS) --silent --get --data-urlencode 'q=serviceName:"tracking-api" AND span.attributes.url@path:"/internal/fleet/positions"' 'http://127.0.0.1:19200/otel-v1-apm-span-*/_search?size=1' | $(JQ) $(JQ_FLAGS) -r '.hits.hits[0]._source["span.attributes.http@response@status_code"] // "unknown"'); \
		if [ "$${span_docs}" -gt 0 ] && [ "$${log_docs}" -gt 0 ] && [ "$${edge_docs}" -gt 0 ] && [ "$${tracking_status}" = '200' ]; then break; fi; \
		sleep 2; \
	done; \
	health=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error 'http://127.0.0.1:19200/_cluster/health' | $(JQ) $(JQ_FLAGS) -r '.status'); \
	tracking_status=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error --get --data-urlencode 'q=serviceName:"tracking-api" AND span.attributes.url@path:"/internal/fleet/positions"' 'http://127.0.0.1:19200/otel-v1-apm-span-*/_search?size=1' | $(JQ) $(JQ_FLAGS) -r '.hits.hits[0]._source["span.attributes.http@response@status_code"] // "unknown"'); \
	printf 'OpenSearch cluster  %s\n' "$${health}"; \
	printf 'Trace documents     %s\n' "$${span_docs}"; \
	printf 'Log documents       %s\n' "$${log_docs}"; \
	printf 'Scenario edge docs  %s\n' "$${edge_docs}"; \
	monitor_body=$$($(JQ) $(JQ_FLAGS) -n --arg name '$(SIEM_MONITOR_NAME)' '{query: {match: {"monitor.name": $$name}}}'); \
	monitor_id=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error --request GET --header 'Content-Type: application/json' --data-raw "$${monitor_body}" 'http://127.0.0.1:19200/_plugins/_alerting/monitors/_search' | $(JQ) $(JQ_FLAGS) -r '.hits.hits[0]._id // empty'); \
	monitor_triggered=$$($(CURL) $(CURL_FLAGS) --fail --silent --show-error --request POST "http://127.0.0.1:19200/_plugins/_alerting/monitors/$${monitor_id}/_execute?dryrun=true" | $(JQ) $(JQ_FLAGS) -r '[.trigger_results[] | select(.name == "unexpected-edge") | .triggered][0] // false'); \
	printf 'Tracking status     %s\n' "$${tracking_status}"; \
	printf 'Unexpected edge     %s\n' "$${monitor_triggered}"; \
	test "$${span_docs}" -gt 0; \
	test "$${log_docs}" -gt 0; \
	test "$${edge_docs}" -gt 0; \
	test -n "$${monitor_id}"; \
	test "$${monitor_triggered}" = true

reset: _check ## Delete the Minikube profile and its local data.
	$(MINIKUBE) $(MINIKUBE_FLAGS) delete --profile $(MINIKUBE_PROFILE)
