// payments-service owns card charging. It hosts the ChargeCard activity and
// the ChargeCustomer workflow, which other apps call cross-app. It has no idea
// who is calling or what they did first: the WorkflowAccessPolicy in
// policy.yaml decides that before this code ever runs.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/dapr/durabletask-go/workflow"
)

type Charge struct {
	OrderID  string  `json:"orderId"`
	Customer string  `json:"customer"`
	Amount   float64 `json:"amount"`
}

func chargeCard(ctx workflow.ActivityContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	fmt.Printf(">>> ChargeCard: %s  $%.2f  %s  CHARGED\n", c.OrderID, c.Amount, c.Customer)
	return fmt.Sprintf("charged $%.2f to %s", c.Amount, c.Customer), nil
}

// chargeCustomer runs ChargeCard locally. Calls an app makes to itself are
// never checked by its own policy.
func chargeCustomer(ctx *workflow.WorkflowContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	var receipt string
	err := ctx.CallActivity("ChargeCard", workflow.WithActivityInput(c)).Await(&receipt)
	return receipt, err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := workflow.NewRegistry()
	if err := r.AddActivityN("ChargeCard", chargeCard); err != nil {
		log.Fatal(err)
	}
	if err := r.AddWorkflowN("ChargeCustomer", chargeCustomer); err != nil {
		log.Fatal(err)
	}

	wf, err := dapr.NewWorkflowClient()
	if err != nil {
		log.Fatal(err)
	}
	if err := wf.StartWorker(ctx, r); err != nil {
		log.Fatal(err)
	}
	fmt.Println("payments-service ready: hosting ChargeCard, ChargeCustomer")
	<-ctx.Done()
}
