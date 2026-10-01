package service

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// runAlert runs one Telegram send in the background with a timeout. A
// panic in it is contained and logged: an alert must never take the
// daemon down (review-20260930-ea34 F1). The channel closes when done.
func runAlert(logger zerolog.Logger, what string, send func(ctx context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logger.Error().Interface("panic", r).Str("alert", what).Msg("Telegram alert panicked; contained")
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := send(ctx); err != nil {
			logger.Warn().Err(err).Str("alert", what).Msg("Telegram alert failed")
		}
	}()
	return done
}
