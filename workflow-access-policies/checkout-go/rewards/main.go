// rewards is a loyalty service that has no business charging cards. Its
// CashbackPayout workflow calls payments-service's ChargeCard anyway, and
// payments-service's policy denies it because rewards is not a listed caller.
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

func cashbackPayout(ctx *workflow.WorkflowContext) (any, error) {
	var c Charge
	if err := ctx.GetInput(&c); err != nil {
		return nil, err
	}
	var receipt string
	err := ctx.CallActivity("ChargeCard",
		workflow.WithActivityAppID("payments-service"),
		workflow.WithActivityInput(c),
		workflow.WithHistoryPropagation(workflow.PropagateOwnHistory()),
	).Await(&receipt)
	return receipt, err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := workflow.NewRegistry()
	if err := r.AddWorkflowN("CashbackPayout", cashbackPayout); err != nil {
		log.Fatal(err)
	}

	wf, err := dapr.NewWorkflowClient()
	if err != nil {
		log.Fatal(err)
	}
	if err := wf.StartWorker(ctx, r); err != nil {
		log.Fatal(err)
	}
	fmt.Println("rewards ready: hosting CashbackPayout")
	<-ctx.Done()
}
