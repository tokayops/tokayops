#!/bin/bash
# Send a FIRING alert to localhost

curl -X POST "http://localhost:8080/webhook/alertmanager?token=${WEBHOOK_SECRET:-mysecret}" \
  -H "Content-Type: application/json" \
  -d '{
  "receiver": "devops-critical",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertgroup": "Blackbox",
        "alertname": "BlackboxProbeFailed",
        "cluster": "k8s-cluster-zen",
        "env": "zen",
        "instance": "logstash-k8s-cluster-zen",
        "severity": "critical",
        "team": "devops"
      },
      "annotations": {
        "dashboard": "https://grafana.example.com/d/xtkCtBkiz/prometheus-blackbox-exporter",
        "runbook": "https://company.atlassian.net/wiki/spaces/OPS/pages/12345/BlackboxProbeFailed",
        "summary": "Blackbox probe failed for logstash-k8s-cluster-zen"
      },
      "startsAt": "2025-12-11T15:14:00Z",
      "generatorURL": "https://vmalert.example.com/...",
      "fingerprint": "504ce894b0347c0d"
    }
  ],
  "groupLabels": {
    "alertname": "BlackboxProbeFailed",
    "severity": "critical"
  },
  "commonLabels": {
    "alertname": "BlackboxProbeFailed",
    "severity": "critical",
    "team": "devops"
  },
  "version": "4",
  "groupKey": "{}/{severity=\"critical\"}:{alertname=\"BlackboxProbeFailed\", severity=\"critical\"}"
}'
echo ""
