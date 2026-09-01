package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This suite exercises the provider-neutral remote Skill repair protocol
// through the real OpenSandbox command and file data planes. It belongs to the
// required Docker-backed live target, not the offline unit-test target.
func TestOpenSandboxRemoteSkillRepairLive(t *testing.T) {
	enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("MANGO_LIVE_OPENSANDBOX")))
	if !enabled {
		t.Skip("set MANGO_LIVE_OPENSANDBOX=1 to run OpenSandbox Skill repair tests")
	}
	providerValue, err := NewOpenSandboxProvider(OpenSandboxConfig{
		BaseURL:  os.Getenv("OPEN_SANDBOX_DOMAIN"),
		APIKey:   os.Getenv("OPEN_SANDBOX_API_KEY"),
		Image:    os.Getenv("OPEN_SANDBOX_IMAGE"),
		UseProxy: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerValue.(*openSandboxProvider)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	_, sandboxValue, err := provider.Create(
		ctx, fmt.Sprintf("skill-repair-%d", time.Now().UnixNano()),
		Spec{Timeout: 2 * time.Minute, Network: "bridge"},
	)
	if err != nil {
		t.Fatal(err)
	}
	box := sandboxValue.(*openSandboxBox)
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	skills := sandboxValue.(SkillBundleSandbox)
	archive, mount := skillArchiveFixture(t, []skillArchiveEntry{{
		name: "Portable_Tool/SKILL.md", body: "Portable Skill\n",
	}})
	if err := skills.ImportReadOnlySkill(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("materialize Skill fixture: %v", err)
	}
	assertLiveRemoteSkillPresent(t, ctx, skills, mount)

	runRoot := func(t *testing.T, script string) {
		t.Helper()
		result, err := box.execMaintenance(
			ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}}, 2*time.Minute,
		)
		if err != nil || result == nil || result.ExitCode != 0 {
			t.Fatalf("root maintenance failed: result=%+v err=%v", result, err)
		}
	}
	marker := remoteSkillMarkerPath(mount)
	staging := remoteSkillMarkerStagingPath(mount)
	sentinel := "/tmp/mango-skill-marker-sentinel"
	corruptions := map[string]string{
		"empty":        ": > " + shellQuote(marker),
		"oversized":    "head -c 16385 /dev/zero > " + shellQuote(marker),
		"invalid-json": "printf 'not-json\\n' > " + shellQuote(marker),
		"directory":    "rm -f " + shellQuote(marker) + "; mkdir " + shellQuote(marker),
		"symlink": "printf unchanged > " + shellQuote(sentinel) +
			"; chmod 0400 " + shellQuote(sentinel) +
			"; rm -f " + shellQuote(marker) + " " + shellQuote(staging) +
			"; ln -s " + shellQuote(sentinel) + " " + shellQuote(marker) +
			"; ln -s " + shellQuote(sentinel) + " " + shellQuote(staging),
	}
	for name, corrupt := range corruptions {
		t.Run("marker-"+name, func(t *testing.T) {
			runRoot(t, "chmod -R u+w "+shellQuote(remoteSkillControlRoot)+
				" 2>/dev/null || true; rm -rf "+shellQuote(marker)+" "+
				shellQuote(staging)+" "+shellQuote(sentinel)+"; "+corrupt)
			present, err := skills.HasReadOnlySkill(ctx, mount)
			if err != nil || present {
				t.Fatalf("corrupt marker presence = %t, %v", present, err)
			}
			if err := skills.ImportReadOnlySkill(
				ctx, mount, bytes.NewReader(archive),
			); err != nil {
				t.Fatalf("repair corrupt marker: %v", err)
			}
			assertLiveRemoteSkillPresent(t, ctx, skills, mount)
			if name == "symlink" {
				runRoot(t, "test \"$(cat "+shellQuote(sentinel)+")\" = unchanged")
			}
		})
	}

	controlSentinelDir := "/tmp/mango-skill-control-target"
	controlSentinel := controlSentinelDir + "/sentinel"
	runRoot(t, "chmod -R u+w "+shellQuote(remoteSkillControlRoot)+
		"; rm -rf "+shellQuote(remoteSkillControlRoot)+" "+shellQuote(controlSentinelDir)+
		"; mkdir "+shellQuote(controlSentinelDir)+
		"; printf unchanged > "+shellQuote(controlSentinel)+
		"; chmod 0400 "+shellQuote(controlSentinel)+
		"; chmod 0500 "+shellQuote(controlSentinelDir)+
		"; ln -s "+shellQuote(controlSentinelDir)+" "+shellQuote(remoteSkillControlRoot))
	if err := skills.ImportReadOnlySkill(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("repair symlinked control root: %v", err)
	}
	runRoot(t, "test ! -L "+shellQuote(remoteSkillControlRoot)+
		" && test -d "+shellQuote(remoteSkillControlRoot)+
		" && test \"$(cat "+shellQuote(controlSentinel)+")\" = unchanged")

	child := mount
	child.Identity = "skill_child@100"
	child.RuntimePath = SessionSkillsRoot + "/.agents/0123456789abcdef01234567/portable-tool"
	childParent := strings.TrimSuffix(child.RuntimePath, "/portable-tool")
	runtimeSentinelDir := "/tmp/mango-skill-runtime-target"
	runtimeSentinel := runtimeSentinelDir + "/sentinel"
	runRoot(t, "mkdir -p "+shellQuote(strings.TrimSuffix(childParent, "/0123456789abcdef01234567"))+
		"; rm -rf "+shellQuote(childParent)+" "+shellQuote(runtimeSentinelDir)+
		"; mkdir "+shellQuote(runtimeSentinelDir)+
		"; printf unchanged > "+shellQuote(runtimeSentinel)+
		"; chmod 0400 "+shellQuote(runtimeSentinel)+
		"; chmod 0500 "+shellQuote(runtimeSentinelDir)+
		"; ln -s "+shellQuote(runtimeSentinelDir)+" "+shellQuote(childParent))
	if err := skills.ImportReadOnlySkill(ctx, child, bytes.NewReader(archive)); err != nil {
		t.Fatalf("repair symlinked runtime parent: %v", err)
	}
	runRoot(t, "test ! -L "+shellQuote(childParent)+" && test -d "+
		shellQuote(childParent)+" && test \"$(cat "+shellQuote(runtimeSentinel)+")\" = unchanged")

	runRoot(t, "chmod -R u+w "+shellQuote(childParent)+
		"; rm -rf "+shellQuote(childParent)+"; printf file > "+shellQuote(childParent))
	if err := skills.ImportReadOnlySkill(ctx, child, bytes.NewReader(archive)); err != nil {
		t.Fatalf("repair non-directory runtime parent: %v", err)
	}
	runRoot(t, "test -d "+shellQuote(childParent)+" && test ! -L "+shellQuote(childParent))
}

func assertLiveRemoteSkillPresent(
	t *testing.T,
	ctx context.Context,
	skills SkillBundleSandbox,
	mount ReadOnlySkillMount,
) {
	t.Helper()
	present, err := skills.HasReadOnlySkill(ctx, mount)
	if err != nil || !present {
		t.Fatalf("HasReadOnlySkill = %t, %v", present, err)
	}
}
