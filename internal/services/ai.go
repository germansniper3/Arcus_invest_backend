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

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
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
		model = "gemini-2.5-flash"
	}

	baseURL := strings.TrimRight(cfg.AIProviderURL, "/")
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", baseURL, model, apiKey)
	prompt := fmt.Sprintf("%s\n\nUser Question: %s\n\nArcus Assist:", system, question)

	reqPayload := geminiRequest{
		Contents: []geminiContent{
			{Parts: []geminiPart{{Text: prompt}}},
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

	resp, err := client.Do(req)
	if err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}
	defer resp.Body.Close()

	var result geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	if result.Error != nil {
		return fallbackAnswer(question), "local-knowledge-base", nil
	}

	if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
		aiText := result.Candidates[0].Content.Parts[0].Text
		return strings.TrimSpace(aiText), "gemini:" + model, nil
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
