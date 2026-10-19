// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fsutil

import (
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

func TestMmapCachedFileUpgradeRetainsOldMapping(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "mmap_cached_file_test")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer tmpFile.Close()

	// Truncate to at least one chunk (chunkSize = 2MB).
	if err := tmpFile.Truncate(int64(chunkSize)); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	var f MmapCachedFile
	f.SetFD(int(tmpFile.Fd()))

	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}

	// Reference without memory accounting and map read-only.
	ar := hostarch.AddrRange{Start: 0, End: hostarch.PageSize}
	f.AddMapping(ar, 0)
	defer f.RemoveMapping(ar, 0)

	safeseqRO, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal (read) failed: %v", err)
	}
	if safeseqRO.IsEmpty() {
		t.Fatalf("MapInternal returned empty Safeseq")
	}

	f.mapsMu.Lock()
	mRO, ok := f.mappings[0]
	f.mapsMu.Unlock()
	if !ok || mRO.writable {
		t.Fatalf("Expected read-only mapping for chunk 0, got: %v, ok=%v", mRO, ok)
	}

	// Upgrade to write.
	safeseqW, err := f.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal (write) failed: %v", err)
	}
	if safeseqW.IsEmpty() {
		t.Fatalf("MapInternal (write) returned empty Safeseq")
	}

	f.mapsMu.Lock()
	mW, ok := f.mappings[0]
	oldAddrs := f.oldMappings[0]
	f.mapsMu.Unlock()

	if !ok || !mW.writable {
		t.Fatalf("Expected writable mapping for chunk 0, got: %v, ok=%v", mW, ok)
	}

	// The old read-only mapping address MUST be retained in oldMappings.
	found := false
	for _, addr := range oldAddrs {
		if addr == mRO.addr {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Old read-only mapping address %#x not retained in oldMappings: %v", mRO.addr, oldAddrs)
	}

	// Test RegenerateMappings.
	if err := f.RegenerateMappings(); err != nil {
		t.Fatalf("RegenerateMappings failed: %v", err)
	}

	f.mapsMu.Lock()
	mRegen, ok := f.mappings[0]
	oldAddrsAfterRegen := f.oldMappings[0]
	f.mapsMu.Unlock()

	if !ok || !mRegen.writable {
		t.Fatalf("Expected writable mapping after regenerate, got: %v, ok=%v", mRegen, ok)
	}

	// mW.addr must now be in oldMappings.
	foundW := false
	for _, addr := range oldAddrsAfterRegen {
		if addr == mW.addr {
			foundW = true
			break
		}
	}
	if !foundW {
		t.Fatalf("Replaced mapping address %#x not retained in oldMappings after RegenerateMappings: %v", mW.addr, oldAddrsAfterRegen)
	}
}
