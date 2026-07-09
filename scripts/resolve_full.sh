#!/bin/bash
# Send a BATCH: Both Resolved (Full Group Resolution)

curl -X POST "http://localhost:8080/webhook/alertmanager?token=${WEBHOOK_SECRET:-mysecret}" \
  -H "Content-Type: application/json" \
  -d '{
  "receiver": "devops-critical",
  "status": "resolved",
  "alerts": [
    {
      "status": "resolved",
      "labels": {
      },
      "annotations": {"summary": "Disk Space Low on db-02"},
      "startsAt": "2024-12-11T20:00:00Z",
      "endsAt": "'"$(date -u +"%Y-%m-%dT%H:%M:%SZ")"'",
      "generatorURL": "http://prometheus:9090",
      "fingerprint": "1002"
    }
  ]
}'
echo ""
