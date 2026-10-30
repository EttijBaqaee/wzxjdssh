package main

import (
	"crypto/rand"
	"math/big"
	"time"
)

func jitterBounds(base time.Duration, percent int) (time.Duration, time.Duration) {
	spread := base * time.Duration(percent) / 100
	low := base - spread
	if low < time.Millisecond {
		low = time.Millisecond
	}
	return low, base + spread
}

func randomDelay(low, high time.Duration) (time.Duration, error) {
	if high <= low {
		return low, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(high-low)+1))
	if err != nil {
		return 0, err
	}
	return low + time.Duration(n.Int64()), nil
}

func jitterDelay(base time.Duration, percent int) (time.Duration, error) {
	low, high := jitterBounds(base, percent)
	return randomDelay(low, high)
}
