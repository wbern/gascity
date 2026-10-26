//go:build !darwin

package main

import (
	"context"
	"os"
)

func archivePlatformHandlesClear(ctx context.Context, root string) error {
	data, err := archiveHandleSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := inspectArchiveHandles(data, root, os.Getpid()); err != nil {
		return err
	}
	return ctx.Err()
}
