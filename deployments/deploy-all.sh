#!/bin/bash
set -e

echo "Provisioning AWS Infrastructure via Terraform..."
cd terraform
terraform init
terraform apply -auto-approve

echo "Configuring kubectl..."
aws eks update-kubeconfig --region ap-south-1 --name iicpc-benchmarking-cluster

echo "Deploying Stateful Dependencies (Redpanda & TimescaleDB via Helm)..."
helm repo add redpanda https://charts.redpanda.com
helm install redpanda redpanda/redpanda -n iicpc-platform --create-namespace
helm repo add timescale https://charts.timescale.com
helm install timescaledb timescale/timescaledb-single -n iicpc-platform

echo "Deploying IICPC Microservices..."
cd ../kubernetes
kubectl apply -f platform-stack.yaml

echo "Deployment Complete. Waiting for LoadBalancers to provision..."
kubectl get svc -n iicpc-platform