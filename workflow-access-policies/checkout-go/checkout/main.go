// checkout runs the order workflows. Charging the card is payments-service's
// job, so both workflows end with a cross-app call to its ChargeCard activity
// and propagate their own signed history along with it.
//
//   - Checkout: fraud-check, then ChargeCard.
//   - CheckoutExpress: a "shortcut" that calls ChargeCard straight away. The
//     policy allows it, because it only checks who is calling. The
//     commented-out `requires` block in policy.yaml would deny it, because the
//     history it carries shows no fraud-check.
package main

import (
	"context"
	"errors"
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

func chargeCard(ctx *workflow.WorkflowContext, c Charge) (string, error) {
	var receipt string
	err := ctx.CallActivity("ChargeCard",
		workflow.WithActivityAppID("payments-service"),
		workflow.WithActivityInput(c),
		workflow.WithHistoryPropagation(workflow.PropagateOwnHistory()),
	).Await(&receipt)
	return receipt, err
}

func checkout(ctx *workflow.WorkflowContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	if err := ctx.CallActivity("fraud-check", workflow.WithActivityInput(c)).Await(nil); err != nil {
		return nil, fmt.Errorf("fraud-check: %w", err)
	}
	return chargeCard(ctx, c)
}

func checkoutExpress(ctx *workflow.WorkflowContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	return chargeCard(ctx, c)
}

func fraudCheck(ctx workflow.ActivityContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	if c.Amount <= 0 {
		return nil, errors.New("invalid amount")
	}
	fmt.Printf(">>> fraud-check: %s  $%.2f  %s  PASSED\n", c.OrderID, c.Amount, c.Customer)
	return "PASSED", nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := workflow.NewRegistry()
	for name, w := range map[string]workflow.Workflow{
		"Checkout":        checkout,
		"CheckoutExpress": checkoutExpress,
	} {
		if err := r.AddWorkflowN(name, w); err != nil {
			log.Fatal(err)
		}
	}
	if err := r.AddActivityN("fraud-check", fraudCheck); err != nil {
		log.Fatal(err)
	}

	wf, err := dapr.NewWorkflowClient()
	if err != nil {
		log.Fatal(err)
	}
	if err := wf.StartWorker(ctx, r); err != nil {
		log.Fatal(err)
	}
	fmt.Println("checkout ready: hosting Checkout, CheckoutExpress")
	<-ctx.Done()
}
