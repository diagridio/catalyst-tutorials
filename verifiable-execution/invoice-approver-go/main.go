// invoice-approver: an invoice approval agent that runs as a durable,
// verifiable Dapr Workflow on Diagrid Catalyst (go-ai + LangChainGo).
//
// One node per business step so each one lands in the signed history:
//
//	get_invoice -> check_vendor -> check_policy -> decide -> release_payment | hold_payment -> END
//
// The checks are plain Go. The model writes the rationale but never gates
// the payment - the router reads the check results directly.
//
// Env:
//
//	OPENAI_API_KEY  required
//	OPENAI_MODEL    optional, default gpt-4o
//	INVOICE_ID      optional, default INV-2026-0311 (ex: INV-2026-0312 gets held)
//	INSTANCE_ID     optional fixed workflow instance ID, handy for export/verify
//
// After the run finishes the process stays up as a workflow worker until
// Ctrl-C, so the instance can be rerun from the console or the CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	goai "github.com/diagridio/go-ai"
	"github.com/diagridio/go-ai/adapters/langchaingo"
	"github.com/diagridio/go-ai/agent"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// graph state - json tags are the channel names
type state struct {
	InvoiceID   string  `json:"invoice_id,omitempty"`
	Vendor      string  `json:"vendor,omitempty"`
	Amount      float64 `json:"amount,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	VendorCheck string  `json:"vendor_check,omitempty"` // APPROVED | FLAGGED
	PolicyCheck string  `json:"policy_check,omitempty"` // PASSED | FAILED
	Review      string  `json:"review,omitempty"`       // prompt for the model
	Decision    string  `json:"decision,omitempty"`     // model's written rationale
	Outcome     string  `json:"outcome,omitempty"`      // RELEASED | HELD
	Receipt     string  `json:"receipt,omitempty"`
}

// mock back-office data

type invoice struct {
	Vendor   string
	Amount   float64
	Currency string
}

var invoices = map[string]invoice{
	"INV-2026-0311": {"Northwind Office Supply", 4200.00, "USD"},
	"INV-2026-0312": {"Acme Consulting LLC", 18750.00, "USD"},
	"INV-2026-0313": {"Bluewater Logistics", 950.00, "USD"},
}

var approvedVendors = map[string]bool{
	"Northwind Office Supply": true,
	"Bluewater Logistics":     true,
}

const policyLimitUSD = 10000.00

// nodes

func getInvoice(_ context.Context, s agent.State) (agent.State, error) {
	id, _ := s["invoice_id"].(string)
	inv, ok := invoices[id]
	if !ok {
		return nil, fmt.Errorf("invoice %q not found", id)
	}
	fmt.Printf(">>> get_invoice: %s vendor=%q amount=%.2f %s\n", id, inv.Vendor, inv.Amount, inv.Currency)
	return agent.State{"vendor": inv.Vendor, "amount": inv.Amount, "currency": inv.Currency}, nil
}

func checkVendor(_ context.Context, s agent.State) (agent.State, error) {
	vendor, _ := s["vendor"].(string)
	result := "FLAGGED"
	if approvedVendors[vendor] {
		result = "APPROVED"
	}
	fmt.Printf(">>> check_vendor: %q -> %s\n", vendor, result)
	return agent.State{"vendor_check": result}, nil
}

func checkPolicy(_ context.Context, s agent.State) (agent.State, error) {
	amount, _ := s["amount"].(float64)
	result := "FAILED"
	if amount <= policyLimitUSD {
		result = "PASSED"
	}
	fmt.Printf(">>> check_policy: %.2f vs limit %.2f -> %s\n", amount, policyLimitUSD, result)

	// build the review prompt from the deterministic results
	review := fmt.Sprintf(
		"Invoice %s from %s for %.2f %s.\nVendor check: %s.\nPolicy check (limit %.2f USD): %s.\n"+
			"Write a two-sentence approval decision for the finance team. If both checks passed, "+
			"say the payment will be released. If either failed, say it is held and why.",
		s["invoice_id"], s["vendor"], amount, s["currency"], s["vendor_check"], policyLimitUSD, result,
	)
	return agent.State{"policy_check": result, "review": review}, nil
}

func releasePayment(_ context.Context, s agent.State) (agent.State, error) {
	id, _ := s["invoice_id"].(string)
	amount, _ := s["amount"].(float64)
	receipt := fmt.Sprintf("PAY-%s-OK", strings.TrimPrefix(id, "INV-"))
	fmt.Printf(">>> release_payment: %.2f for %s -> %s\n", amount, id, receipt)
	return agent.State{"outcome": "RELEASED", "receipt": receipt}, nil
}

func holdPayment(_ context.Context, s agent.State) (agent.State, error) {
	id, _ := s["invoice_id"].(string)
	fmt.Printf(">>> hold_payment: %s held for manual review\n", id)
	return agent.State{"outcome": "HELD", "receipt": "none"}, nil
}

// the checks gate the payment, not the model
func route(_ context.Context, s agent.State) (string, error) {
	if s["vendor_check"] == "APPROVED" && s["policy_check"] == "PASSED" {
		return "release", nil
	}
	return "hold", nil
}

func main() {
	ctx := context.Background()
	model := pickModel()

	graph := agent.NewGraph("invoice-approver").
		AddNode("get_invoice", getInvoice).
		AddNode("check_vendor", checkVendor).
		AddNode("check_policy", checkPolicy).
		AddNode("decide", langchaingo.ModelNode(model,
			langchaingo.WithSystemPrompt("You are an accounts-payable approval assistant. Be precise and brief."),
			langchaingo.WithInputKey("review"),
			langchaingo.WithOutputKey("decision"),
		)).
		AddNode("release_payment", releasePayment).
		AddNode("hold_payment", holdPayment).
		SetEntry("get_invoice").
		AddEdge("get_invoice", "check_vendor").
		AddEdge("check_vendor", "check_policy").
		AddEdge("check_policy", "decide").
		AddConditionalEdge("decide", route, map[string]string{
			"release": "release_payment",
			"hold":    "hold_payment",
		}).
		AddEdge("release_payment", agent.END).
		AddEdge("hold_payment", agent.END)

	runner, err := goai.NewRunner(ctx, goai.Config{
		Graph:     graph,
		Name:      "invoice-approver",
		Framework: langchaingo.Framework,
		MaxSteps:  20,
		Role:      "Invoice Payment Approver",
		Goal:      "Verify vendor and policy before releasing invoice payments",
		Tools: []goai.ToolInfo{
			{Name: "get_invoice", Description: "Fetch an invoice by ID"},
			{Name: "check_vendor", Description: "Check the vendor against the approved-vendor list"},
			{Name: "check_policy", Description: "Check the amount against the auto-approval limit"},
			{Name: "release_payment", Description: "Release payment for an approved invoice"},
		},
	})
	if err != nil {
		fatal(err)
	}
	defer runner.Close()

	out, err := runner.Invoke(ctx, state{InvoiceID: env("INVOICE_ID", "INV-2026-0311")}, invokeOpts()...)
	if err != nil {
		fatal(err)
	}

	var result state
	err = out.Into(&result)
	if err != nil {
		fatal(err)
	}
	fmt.Println()
	fmt.Printf("Agent %q (framework=%s) ran as workflow %s\n", runner.Name(), runner.Framework(), runner.WorkflowName())
	fmt.Printf("Invoice  : %s  %s  %.2f %s\n", result.InvoiceID, result.Vendor, result.Amount, result.Currency)
	fmt.Printf("Checks   : vendor=%s policy=%s\n", result.VendorCheck, result.PolicyCheck)
	fmt.Printf("Decision : %s\n", result.Decision)
	fmt.Printf("Outcome  : %s (receipt %s)\n", result.Outcome, result.Receipt)

	// Stay connected as a workflow worker so the instance can be rerun from
	// the console or `diagrid workflow rerun` - reruns need a host for the
	// workflow actor. Ctrl-C when you're done.
	fmt.Println()
	fmt.Println("Worker still connected - rerun or tamper now. Press Ctrl-C to stop.")
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

// pin a fixed instance ID when INSTANCE_ID is set so you know what to export
func invokeOpts() []goai.InvokeOptions {
	id := os.Getenv("INSTANCE_ID")
	if id != "" {
		return []goai.InvokeOptions{{InstanceID: id}}
	}
	return nil
}

func pickModel() llms.Model {
	if os.Getenv("OPENAI_API_KEY") == "" {
		fatal(fmt.Errorf("OPENAI_API_KEY is not set"))
	}
	m, err := openai.New(openai.WithModel(env("OPENAI_MODEL", "gpt-4o")))
	if err != nil {
		fatal(fmt.Errorf("openai model: %w", err))
	}
	return m
}

func env(key, def string) string {
	v := os.Getenv(key)
	if v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
