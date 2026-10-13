package main

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"time"
)

// Charge the complete maximum target, not merely its increment: three 20 GiB
// volumes round up to at most 66 provider GB. Gross EUR pricing must fit within
// the approved EUR 5 monthly increment even under this conservative bound.
func priceWithinBudget(b []byte) bool {
	response, err := decode(b)
	gross := str(field(response, "pricing", "volume", "price_per_gb_month", "gross"))
	if err != nil || field(response, "pricing", "currency") != "EUR" || !regexp.MustCompile(`^[0-9]{1,8}(\.[0-9]{1,12})?$`).MatchString(gross) {
		return false
	}
	price, ok := new(big.Rat).SetString(gross)
	if !ok || price.Sign() <= 0 {
		return false
	}
	return new(big.Rat).Mul(price, big.NewRat(66, 1)).Cmp(big.NewRat(5, 1)) <= 0
}

func fetchPrice(ctx context.Context, client *http.Client, token string) bool {
	if token == "" {
		return false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/pricing", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	b, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	closeErr := response.Body.Close()
	return response.StatusCode == http.StatusOK && readErr == nil && closeErr == nil && priceWithinBudget(b)
}

func priceAllowed(ctx context.Context) bool {
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return fetchPrice(ctx, client, os.Getenv("HCLOUD_TOKEN"))
}
