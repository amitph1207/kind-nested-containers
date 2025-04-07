#!/bin/bash
# setup_control_plane.sh

# Create kind cluster
kind create cluster --name test-cluster

# Wait for cluster to be ready
sleep 10

# Get the control plane node IP
CONTROL_PLANE_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' kind-control-plane)

# Get the CA cert hash
CA_CERT_HASH=$(kubectl config view --context kind-kind --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d | openssl x509 -pubkey -noout | openssl rsa -pubin -outform der 2>/dev/null | openssl dgst -sha256 -hex | sed 's/^.* //')

# Get the join token
JOIN_TOKEN=$(kubeadm token create)

echo "Control Plane IP: $CONTROL_PLANE_IP"
echo "CA Cert Hash: $CA_CERT_HASH"
echo "Join Token: $JOIN_TOKEN"
