package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	PrefixAgent           = "agent_"
	PrefixEnv             = "env_"
	PrefixSession         = "sesn_"
	PrefixSessionThread   = "sthr_"
	PrefixContextSnapshot = "csnp_"
	PrefixEvent           = "sevt_"
	PrefixRun             = "run_"
	PrefixOutcome         = "outc_"
	PrefixFile            = "file_"
	PrefixSessionResource = "sesrsc_"
	PrefixSkill           = "skill_"
	PrefixMemoryStore     = "memstore_"
	PrefixMemory          = "mem_"
	PrefixMemoryVersion   = "memver_"
	PrefixVault           = "vlt_"
	PrefixVaultCredential = "vcrd_"
	PrefixDeployment      = "depl_"
	PrefixDeploymentRun   = "drun_"
	PrefixEnvironmentWork = "work_"
	PrefixWebhook         = "wh_"
	PrefixWebhookEvent    = "whe_"
	PrefixWebhookClaim    = "whclaim_"
)

type Clock interface{ Now() time.Time }

type FixedClock struct{ T time.Time }

func (c FixedClock) Now() time.Time { return c.T }

type IDGenerator interface{ NewID(prefix string) string }

type SeqIDGen struct {
	mu sync.Mutex
	n  map[string]int
}

func NewSeqIDGen() *SeqIDGen { return &SeqIDGen{n: map[string]int{}} }

func (g *SeqIDGen) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[prefix]++
	return fmt.Sprintf("%s%d", prefix, g.n[prefix])
}

// RandomIDGen produces process-unique ids like "agent_9f8c1a2b3c4d5e6f" using
// crypto/rand. Suitable for the real server where ids must not collide across
// restarts (unlike SeqIDGen, whose counter resets each process start).
type RandomIDGen struct{}

// Compile-time assertion that RandomIDGen satisfies IDGenerator.
var _ IDGenerator = (*RandomIDGen)(nil)

func NewRandomIDGen() *RandomIDGen { return &RandomIDGen{} }

func (RandomIDGen) NewID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error on supported platforms
	return prefix + hex.EncodeToString(b[:])
}
