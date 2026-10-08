package fsys

import (
	"os"
	"reflect"
)

// SameFileMetadata reports whether identity and content-change metadata agree.
// It requires file identity, size, mtime and a real inode change timestamp.
// Unsupported metadata (including Windows OS stat data) returns false so callers
// reload rather than treating size and mtime alone as evidence of freshness. Metadata can collide on coarse filesystems;
// this is a cache heuristic, not a byte-equality guarantee.
func SameFileMetadata(first, second os.FileInfo) bool {
	if first == nil || second == nil || !SameFileIdentity(first, second) {
		return false
	}
	firstTime, firstOK := changeTime(first)
	secondTime, secondOK := changeTime(second)
	return firstOK && secondOK && firstTime == secondTime && first.Size() == second.Size() && first.ModTime().Equal(second.ModTime())
}

type fileChangeTime struct{ sec, nsec int64 }

// changeTime uses the Unix Ctim/Ctimespec shapes and Fake's synthetic Ctime.
// Keeping seconds and nanoseconds separate avoids overflow for wide timestamps.
func changeTime(info os.FileInfo) (fileChangeTime, bool) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() || stat.Kind() != reflect.Struct {
		return fileChangeTime{}, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := stat.FieldByName(name)
		if field.IsValid() && field.Kind() == reflect.Struct {
			sec, secOK := signedStatField(field.FieldByName("Sec"))
			nsec, nsecOK := signedStatField(field.FieldByName("Nsec"))
			if secOK && nsecOK {
				return fileChangeTime{sec, nsec}, true
			}
		}
	}
	if nanos, ok := signedStatField(stat.FieldByName("Ctime")); ok {
		return fileChangeTime{nanos / 1e9, nanos % 1e9}, true
	}
	return fileChangeTime{}, false
}

func signedStatField(field reflect.Value) (int64, bool) {
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	default:
		return 0, false
	}
}
