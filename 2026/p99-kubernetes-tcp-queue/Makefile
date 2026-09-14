.DEFAULT_GOAL := all

.PHONY: all test prometheus-ui clean

IMAGE ?= localhost/tcp-notsent-poc:e2e
IMAGE_TAR ?= /tmp/tcp-notsent-poc-e2e.tar
MINIKUBE_PROFILE ?= p99-tcp-queue
KUBERNETES_VERSION ?= v1.37.0
PROMETHEUS_CHART_VERSION ?= 25.28.0
KUBECTL := kubectl --context $(MINIKUBE_PROFILE)
HELM := helm --kube-context $(MINIKUBE_PROFILE)

all:
	@for tool in go podman minikube kubectl helm jq; do \
		command -v "$$tool" >/dev/null || { echo "missing required tool: $$tool"; exit 1; }; \
	done
	$(GOENV) go test -count=1 ./...
	minikube delete -p $(MINIKUBE_PROFILE) || true
	minikube start -p $(MINIKUBE_PROFILE) --driver=docker --kubernetes-version=$(KUBERNETES_VERSION)
	podman build -t $(IMAGE) -f Containerfile .
	rm -f $(IMAGE_TAR)
	podman save -o $(IMAGE_TAR) $(IMAGE)
	minikube -p $(MINIKUBE_PROFILE) image load $(IMAGE_TAR)
	$(KUBECTL) apply -f kubernetes.yml
	$(HELM) upgrade --install prometheus prometheus \
		--repo https://prometheus-community.github.io/helm-charts --version $(PROMETHEUS_CHART_VERSION) \
		--namespace tcp-notsent-poc --create-namespace --wait --timeout 5m \
		-f prometheus-values.yml
	POC_KUBE_CONTEXT=$(MINIKUBE_PROFILE) $(GOENV) go test -run '^TestE2E$$' -e2e -count=1 -timeout=30m -v .
	jq '{ok,time,baseline:(.baseline|{lowat,scenario_p99_ms,max_notsent,bulk_MBps,conns,scenario_samples}),"128k":(."128k"|{lowat,scenario_p99_ms,max_notsent,bulk_MBps,conns,scenario_samples}),"4k":(."4k"|{lowat,scenario_p99_ms,max_notsent,bulk_MBps,conns,scenario_samples})}' result.json

test:
	$(GOENV) go test -count=1 ./...

prometheus-ui:
	$(KUBECTL) -n tcp-notsent-poc port-forward svc/prometheus-server 9090:80

clean:
	$(HELM) uninstall prometheus --namespace tcp-notsent-poc --ignore-not-found || true
	$(KUBECTL) delete -f kubernetes.yml --ignore-not-found || true
	minikube delete -p $(MINIKUBE_PROFILE) || true
