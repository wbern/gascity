//go:build !darwin

package main

import (
	"context"
	"os"
)

func archivePlatformHandlesClear(ctx context.Context, root string) error {
	return retryArchiveCensus(ctx, func(data []byte) error {
		return inspectArchiveHandles(data, root, os.Getpid())
	})
}
