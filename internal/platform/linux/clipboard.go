//go:build linux

package linux

import (
	"context"
	"errors"

	"github.com/titrom/rmouse/internal/proto"
)

type Clipboard struct{}

func NewClipboard() (*Clipboard, error) {
	return nil, errors.New("platform/linux: clipboard sync is not implemented yet")
}

func (*Clipboard) Read() (proto.ClipboardFormat, []byte, bool, error) {
	return 0, nil, false, errors.New("platform/linux: clipboard sync is not implemented yet")
}

func (*Clipboard) Write(proto.ClipboardFormat, []byte) error {
	return errors.New("platform/linux: clipboard sync is not implemented yet")
}

func (*Clipboard) Watch(context.Context, func(proto.ClipboardFormat, []byte)) error {
	return errors.New("platform/linux: clipboard sync is not implemented yet")
}

func (*Clipboard) Close() error { return nil }
