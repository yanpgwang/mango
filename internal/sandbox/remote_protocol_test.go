package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type snapshotCleanupDouble struct {
	remoteFileResourceDataPlane
	removed string
}

func (*snapshotCleanupDouble) ResourceStat(
	context.Context,
	string,
) (remoteFileInfo, error) {
	return remoteFileInfo{Directory: true}, nil
}

func (f *snapshotCleanupDouble) ResourceRemoveFile(
	_ context.Context,
	name string,
) error {
	f.removed = name
	return nil
}

func TestRemoteSessionOutputArchiveTimeoutIsRetryable(t *testing.T) {
	handle := &snapshotCleanupDouble{}
	resources := newRemoteFileResources("test", handle)
	var archivePath string
	stream, err := openRemoteSessionOutputs(
		context.Background(),
		"test",
		resources,
		func(_ context.Context, command Command) (*Result, error) {
			archivePath = command.Args[3]
			return &Result{ExitCode: -1, TimedOut: true}, nil
		},
	)
	if stream != nil {
		_ = stream.Close()
		t.Fatal("timed-out Session output archive returned a stream")
	}
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("archive error = %v, want retryable timeout", err)
	}
	if archivePath == "" || handle.removed != archivePath {
		t.Fatalf("archive cleanup = %q, want %q", handle.removed, archivePath)
	}
}

func TestRemoteOperationCommandTimeoutUsesCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got := remoteOperationCommandTimeout(ctx, 5*time.Second)
	if got < 50*time.Second || got > time.Minute {
		t.Fatalf("operation timeout = %v, want caller deadline", got)
	}
	if got := remoteOperationCommandTimeout(context.Background(), 5*time.Second); got != 5*time.Second {
		t.Fatalf("fallback timeout = %v, want 5s", got)
	}
}

func TestRemoteSkillCommandFailureDiagnostics(t *testing.T) {
	reconciler := &remoteSkillBundles{
		provider: OpenSandboxProviderName,
		execute: func(context.Context, Command) (*Result, error) {
			return &Result{
				ExitCode: 8,
				Stdout:   []byte("ordinary output"),
				Stderr:   []byte("specific failure"),
			}, nil
		},
	}
	err := reconciler.runCommand(context.Background(), "publish Skill", "true")
	if err == nil || !IsPermanent(err) ||
		!strings.Contains(err.Error(), "specific failure") ||
		strings.Contains(err.Error(), "ordinary output") {
		t.Fatalf("runCommand error = %v", err)
	}
}

func TestRemoteSkillCommandTimeoutIsRetryable(t *testing.T) {
	reconciler := &remoteSkillBundles{
		provider: OpenSandboxProviderName,
		execute: func(context.Context, Command) (*Result, error) {
			return &Result{ExitCode: -1, TimedOut: true}, nil
		},
	}
	err := reconciler.runCommand(context.Background(), "publish Skill", "true")
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runCommand timeout = %v, want retryable", err)
	}
}

type remoteSkillMarkerDouble struct {
	remoteFileResourceDataPlane
	info remoteFileInfo
	data []byte
}

func (f *remoteSkillMarkerDouble) ResourceStat(
	context.Context,
	string,
) (remoteFileInfo, error) {
	return f.info, nil
}

func (f *remoteSkillMarkerDouble) ResourceOpen(
	context.Context,
	string,
) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (*remoteSkillMarkerDouble) ResourceIsNotFound(error) bool { return false }

func TestRemoteSkillRejectsCorruptMarkerShapes(t *testing.T) {
	mount := ReadOnlySkillMount{
		Identity: "skill_portable@100", Name: "portable-tool",
		RuntimePath: SessionSkillsRoot + "/portable-tool", ArchiveRoot: "Portable_Tool",
		SizeBytes: 1, UncompressedSizeBytes: 1,
		ChecksumSHA256: strings.Repeat("a", 64),
	}
	tests := map[string]*remoteSkillMarkerDouble{
		"empty":        {info: remoteFileInfo{Regular: true}},
		"oversized":    {info: remoteFileInfo{Regular: true, SizeBytes: remoteSkillMarkerLimit + 1}},
		"invalid-json": {info: remoteFileInfo{Regular: true, SizeBytes: 9}, data: []byte("not-json\n")},
		"directory":    {info: remoteFileInfo{Directory: true}},
		"symlink":      {info: remoteFileInfo{}},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			skills := newRemoteSkillBundles(
				"test", newRemoteFileResources("test", files),
				func(context.Context, Command) (*Result, error) {
					return nil, errors.New("invalid marker must not invoke repair")
				},
			)
			present, err := skills.HasReadOnlySkill(context.Background(), mount)
			if err != nil || present {
				t.Fatalf("corrupt marker presence = %t, %v", present, err)
			}
		})
	}
}
