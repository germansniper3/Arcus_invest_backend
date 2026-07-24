package services

import (
	"arcusinvest/internal/config"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func Chat(cfg *config.Config, question string) (string, string, error) {
	system := "You are Arcus Assist, the official AI assistant for Arcus Investments in Kitwe, Zambia. You provide helpful information about our engineering services (custom PCB design, mechanical fabrication, machine shop, assembly), the products in our engineering product catalogue, and our Innovation Hub student training program. Keep your responses concise, professional, and practical. Do not invent specific products, prices, or availability — direct users to the product catalogue for those details."

	// No API key configured: serve the local knowledge base rather than
	// calling the provider.
	apiKey := cfg.AIAPIKey
	if apiKey == "" {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	model := cfg.AIModel
	if model == "" {
		model = "claude-sonnet-5"
	}

	baseURL := strings.TrimRight(cfg.AIProviderURL, "/")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	url := fmt.Sprintf("%s/v1/messages", baseURL)

	reqPayload := anthropicRequest{
		Model:     model,
		MaxTokens: 1024,
		System:    system,
		Messages: []anthropicMessage{
			{Role: "user", Content: question},
		},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}
	defer resp.Body.Close()

	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	if result.Error != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	if len(result.Content) > 0 && result.Content[0].Text != "" {
		return strings.TrimSpace(result.Content[0].Text), "anthropic:" + model, nil
	}

	return fallbackAnswer(question), "local-knowledge-base", nil
}

func fallbackAnswer(question string) string {
	q := strings.ToLower(question)
	if strings.Contains(q, "enroll") || strings.Contains(q, "student") || strings.Contains(q, "hub") || strings.Contains(q, "apply") {
		return "Arcus Innovation Hub enrollment is open! You can submit an application via our Enrollment page. Our program has three tiers: Explorer (for starting ideas), Builder (for prototyping), and Professional (for advanced capstone designs). Once reviewed, you will receive an invitation to set up your account."
	}
	if strings.Contains(q, "quote") || strings.Contains(q, "price") || strings.Contains(q, "cost") || strings.Contains(q, "budget") {
		return "For pricing details or scoped engineering quotes, please fill out our 'Get a Quote' form on the homepage. Our engineers will review your specifications (electronics, mechanics, software, fabrication) and follow up with a formal estimate."
	}
	if strings.Contains(q, "pcb") || strings.Contains(q, "electronics") || strings.Contains(q, "board") {
		return "Arcus Investments specializes in local custom PCB design, reflow soldering population, debugging, and firmware programming. We help innovators and companies move physical hardware from concept to functional prototype and small-batch production."
	}
	if strings.Contains(q, "product") || strings.Contains(q, "catalogue") || strings.Contains(q, "catalog") || strings.Contains(q, "buy") || strings.Contains(q, "ebike") || strings.Contains(q, "bike") || strings.Contains(q, "mobility") {
		return "You can browse our current engineering products, including any electric mobility platforms, on the Products page. Each catalogue entry lists up-to-date specifications, pricing, and availability. Our products are engineered for local conditions and designed to be maintainable and repairable in Zambia."
	}
	return "Arcus Investments is a Kitwe, Zambia-based engineering firm providing complete electronic maintenance, PCB prototyping, mechanical fabrication (CNC milling, lathe work, welding), software integration, and innovation mentorship. How can we help you today?"
}
