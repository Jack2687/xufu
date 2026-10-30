package platform

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Keep the historical hooks, but never rotate an order's original proxy.
// Expiry or a missing binding stops either flow, even before submission.
func (a *App) restartCheckoutNetworkTx(context.Context, pgx.Tx, string, string) (bool, error) {
	return false, nil
}
func (a *App) restartPaymentNetwork(context.Context, string, string, paymentEvent) (bool, error) {
	return false, nil
}
