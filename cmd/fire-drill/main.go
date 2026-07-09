package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	webhookURL := flag.String("url", "http://localhost:8080/webhook/alertmanager", "Webhook URL")
	token := flag.String("token", os.Getenv("WEBHOOK_SECRET"), "Webhook Token")
	teamName := flag.String("team", "devops", "Team to test")
	severity := flag.String("severity", "critical", "Severity to test") // Default critical

	flag.Parse()

	if *token == "" {
		log.Fatal("Token is required. Set WEBHOOK_SECRET env or use --token")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s?token=%s", *webhookURL, *token)

	log.Printf("🔥 Starting Fire Drill against %s for Team: %s, Severity: %s", *webhookURL, *teamName, *severity)

	if err := runScenario(client, url, *teamName, *severity); err != nil {
		log.Printf("❌ Scenario Failed: %v", err)
	} else {
		log.Printf("✅ Scenario Completed")
	}
	log.Println("🏁 Fire Drill Finished")
}

func runScenario(client *http.Client, url, team, severity string) error {
	fingerprint1 := fmt.Sprintf("drill-%s-%s-1", team, severity)
	fingerprint2 := fmt.Sprintf("drill-%s-%s-2", team, severity)

	// Phase 1: Fire Both
	log.Println("  >> Phase 1: Firing 2 Alerts...")
	payload1 := buildPayload(team, severity, "firing", "firing", fingerprint1, fingerprint2)
	if err := send(client, url, payload1); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)

	// Phase 2: Partial Resolve (1 Resolved, 1 Firing)
	log.Println("  >> Phase 2: Partial Resolve (1 Res, 1 Fire)...")
	payload2 := buildPayload(team, severity, "resolved", "firing", fingerprint1, fingerprint2)
	if err := send(client, url, payload2); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)

	// Phase 3: Full Resolve
	log.Println("  >> Phase 3: Full Resolve...")
	payload3 := buildPayload(team, severity, "resolved", "resolved", fingerprint1, fingerprint2)
	if err := send(client, url, payload3); err != nil {
		return err
	}

	return nil
}

func send(client *http.Client, url string, payload interface{}) error {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func buildPayload(team, severity, status1, status2, fp1, fp2 string) map[string]interface{} {
	// Status for list
	mainStatus := "firing"
	if status1 == "resolved" && status2 == "resolved" {
		mainStatus = "resolved"
	}

	return map[string]interface{}{
		"version":  "4",
		"status":   mainStatus,
		"receiver": "webhook",
		"groupKey": fmt.Sprintf("{}:{team=\"%s\", severity=\"%s\"}", team, severity),
		"groupLabels": map[string]string{
			"team":     team,
			"severity": severity,
		},
		"commonLabels": map[string]string{
			"team":      team,
			"severity":  severity,
			"alertname": "FireDrillTest",
		},
		"alerts": []interface{}{
			buildAlert(team, severity, status1, fp1, "Instance-A", "1"),
			buildAlert(team, severity, status2, fp2, "Instance-B", "2"),
		},
	}
}

func buildAlert(team, severity, status, fp, instance, suffix string) map[string]interface{} {
	start := time.Now().UTC()
	end := time.Time{}
	if status == "resolved" {
		end = time.Now().UTC()
	}

	return map[string]interface{}{
		"status": status,
		"labels": map[string]string{
			"team":      team,
			"severity":  severity,
			"alertname": "FireDrillTest",
			"instance":  instance,
			"drill_id":  suffix,
		},
		"annotations": map[string]string{
			"summary":   fmt.Sprintf("Fire Drill Test Alert %s for %s/%s", suffix, team, severity),
			"dashboard": "https://example.com/drill",
			"runbook":   "https://example.com/runbook",
		},
		"startsAt":    start.Format(time.RFC3339),
		"endsAt":      end.Format(time.RFC3339),
		"fingerprint": fp,
	}
}
