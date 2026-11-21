package gha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	signingconfig "github.com/moby/buildkit/cache/remotecache/signing/config"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/moby/buildkit/util/testutil/workers"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func init() {
	if workers.IsTestDockerd() {
		workers.InitDockerdWorker()
	} else {
		workers.InitOCIWorker()
		workers.InitContainerdWorker()
	}
}

func TestGhaCacheIntegration(t *testing.T) {
	integration.Run(t,
		integration.TestFuncs(testBasicGhaCacheImportExportExtraTimeout),
		integration.WithMirroredImages(integration.OfficialImages("busybox:latest")),
	)
}

func testBasicGhaCacheImportExportExtraTimeout(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendGha,
	)

	c, err := client.New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n foobar > const"`)
	run(`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	var cacheVersion string
	if v, ok := os.LookupEnv("ACTIONS_CACHE_SERVICE_V2"); ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			cacheVersion = "2"
		}
	}

	cacheAttrs := map[string]string{}
	if cacheVersion == "2" {
		cacheAttrs["url_v2"] = os.Getenv("ACTIONS_RESULTS_URL")
	}
	cacheAttrs["url"] = os.Getenv("ACTIONS_CACHE_URL")
	if cacheAttrs["url"] == "" {
		cacheAttrs["url"] = os.Getenv("ACTIONS_RESULTS_URL")
	}
	cacheAttrs["token"] = os.Getenv("ACTIONS_RUNTIME_TOKEN")

	if cacheAttrs["token"] == "" || (cacheAttrs["url"] == "" && cacheAttrs["url_v2"] == "") {
		t.Skip("actions runtime token and cache url must be set")
	}

	scope := "buildkit-" + t.Name()
	if ref := os.Getenv("GITHUB_REF"); ref != "" {
		if after, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			scope += "-" + after
		} else if after, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			scope += "-" + after
		} else if strings.HasPrefix(ref, "refs/pull/") {
			scope += "-pr" + strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(ref, "/head"), "/merge"), "refs/pull/")
		}
	}

	cacheExportAttrs := map[string]string{
		"scope": scope,
		"mode":  "max",
	}
	maps.Copy(cacheExportAttrs, cacheAttrs)

	_, err = c.Solve(sb.Context(), def, client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheExports: []client.CacheOptionsEntry{{
			Type:  "gha",
			Attrs: cacheExportAttrs,
		}},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "const"))
	require.NoError(t, err)
	require.Equal(t, "foobar", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)

	destDir = t.TempDir()

	cacheImportAttrs := map[string]string{
		"scope": scope,
	}
	maps.Copy(cacheImportAttrs, cacheAttrs)

	// Github Cache Service v2 has a problem of not being immediately consistent
	// so we need to wait for the changes to propagate.
	time.Sleep(3 * time.Second)

	_, err = c.Solve(sb.Context(), def, client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: []client.CacheOptionsEntry{{
			Type:  "gha",
			Attrs: cacheImportAttrs,
		}},
	}, nil)
	require.NoError(t, err)

	dt2, err := os.ReadFile(filepath.Join(destDir, "const"))
	require.NoError(t, err)
	require.Equal(t, "foobar", string(dt2))

	dt2, err = os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)
	require.Equal(t, string(dt), string(dt2))
}

func ensurePruneAll(t *testing.T, c *client.Client, sb integration.Sandbox) {
	for i := range 2 {
		require.NoError(t, c.Prune(sb.Context(), nil, client.PruneAll))
		for range 20 {
			du, err := c.DiskUsage(sb.Context())
			require.NoError(t, err)
			if len(du) == 0 {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("retrying prune(%d)", i)
	}
	t.Fatal("failed to ensure prune")
}

func requiresLinux(t *testing.T) {
	integration.SkipOnPlatform(t, "!linux")
}

func TestIndexKeySigning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config signingconfig.Config
		key    string
	}{
		{
			name: "unsigned",
			key:  "index-buildkit-1-e3b0c442",
		},
		{
			name: "signing",
			config: signingconfig.Config{
				Sign: &signingconfig.SignConfig{Command: []string{"sign-cache"}},
			},
			key: "index-buildkit-1-e3b0c442-sig",
		},
		{
			name: "required verification",
			config: signingconfig.Config{
				Verify: signingconfig.VerifyConfig{Required: true},
			},
			key: "index-buildkit-1-e3b0c442-sig",
		},
		{
			name: "legacy empty signing command",
			config: signingconfig.Config{
				Sign: &signingconfig.SignConfig{},
			},
			key: "index-buildkit-1-e3b0c442-sig",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := getConfig(&tc.config, nil, map[string]string{
				attrToken: "unused",
				attrURL:   "https://cache.example",
			})
			require.NoError(t, err)
			require.Equal(t, tc.key, indexKey("", cfg))
		})
	}
}

type cacheVerifierFunc func(context.Context, digest.Digest, []byte) error

func (f cacheVerifierFunc) Verify(ctx context.Context, dgst digest.Digest, bundle []byte) error {
	return f(ctx, dgst, bundle)
}

func TestLoadScopeSignature(t *testing.T) {
	for _, tc := range []struct {
		name          string
		required      bool
		missingIndex  bool
		missingBundle bool
		rejectBundle  bool
		wantErr       string
	}{
		{
			name: "unsigned",
		},
		{
			name:         "absent signed cache",
			required:     true,
			missingIndex: true,
		},
		{
			name:          "missing signature",
			required:      true,
			missingBundle: true,
			wantErr:       "missing signature for github actions cache",
		},
		{
			name:         "verify before parsing",
			required:     true,
			rejectBundle: true,
			wantErr:      "signature rejected",
		},
		{
			name:     "verified signature",
			required: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("{\n  \"records\": []\n}\n")
			if tc.rejectBundle {
				payload = []byte("invalid JSON must not be parsed before verification")
			}
			bundle := []byte("test signature bundle")
			claims, err := json.Marshal(map[string]any{
				"ac":  `[{"Scope":"refs/heads/main","Permission":1}]`,
				"nbf": time.Now().Add(-time.Minute).Unix(),
				"exp": time.Now().Add(time.Hour).Unix(),
			})
			require.NoError(t, err)
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
			cfg, err := getConfig(&signingconfig.Config{
				Verify: signingconfig.VerifyConfig{Required: tc.required},
			}, nil, map[string]string{
				attrToken:      token,
				attrURL:        "https://cache.example",
				attrAPIVersion: "1",
			})
			require.NoError(t, err)
			index := indexKey("refs/heads/main", cfg)
			sigKey := "buildkit-blob-1-" + digest.FromBytes(payload).String() + "-sig"
			var signatureLookups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/_apis/artifactcache/cache":
					key := r.URL.Query().Get("keys")
					var path string
					switch key {
					case index:
						if !tc.missingIndex {
							path = "/index"
						}
					case sigKey:
						signatureLookups.Add(1)
						if !tc.missingBundle {
							path = "/signature"
						}
					default:
						t.Errorf("unexpected cache key %q", key)
					}
					if path == "" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if err := json.NewEncoder(w).Encode(map[string]string{
						"cacheKey":        key,
						"archiveLocation": "http://" + r.Host + path,
					}); err != nil {
						t.Error(err)
					}
				case "/index":
					_, _ = w.Write(payload)
				case "/signature":
					_, _ = w.Write(bundle)
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg.URL = server.URL
			verified := false
			cfg.verifier = cacheVerifierFunc(func(_ context.Context, dgst digest.Digest, got []byte) error {
				verified = true
				require.Equal(t, digest.FromBytes(payload), dgst)
				require.Equal(t, bundle, got)
				if tc.rejectBundle {
					return errors.New("signature rejected")
				}
				return nil
			})
			ci, err := NewImporter(cfg)
			require.NoError(t, err)
			chains, err := ci.(*importer).loadScope(t.Context(), "refs/heads/main")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, chains)
			} else {
				require.NoError(t, err)
				require.NotNil(t, chains)
			}
			require.Equal(t, tc.required && !tc.missingIndex && !tc.missingBundle, verified)
			if tc.required && !tc.missingIndex {
				require.Equal(t, int32(1), signatureLookups.Load())
			} else {
				require.Zero(t, signatureLookups.Load())
			}
		})
	}
}

func TestGhaCacheMountsIntegration(t *testing.T) {
	integration.Run(t,
		integration.TestFuncs(testGhaCacheMountsImportExport),
		integration.WithMirroredImages(integration.OfficialImages("busybox:latest")),
	)
}

func testGhaCacheMountsImportExport(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendGha,
	)

	c, err := client.New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	cacheAttrs := ghaCacheAttrs(t)
	scope := "buildkit-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	busybox := llb.Image("busybox:latest")
	solve := func(cmd string, cacheExports, cacheImports []client.CacheOptionsEntry) string {
		st := busybox.Run(
			llb.Shlex(`sh -c "`+cmd+`"`),
			llb.AddMount("/cache", llb.Scratch(), llb.AsPersistentCacheDir("gha-mounts-test", llb.CacheMountShared)),
			llb.Dir("/wd"),
		).AddMount("/wd", llb.Scratch())
		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)
		destDir := t.TempDir()
		_, err = c.Solve(sb.Context(), def, client.SolveOpt{
			Exports: []client.ExportEntry{{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			}},
			CacheExports: cacheExports,
			CacheImports: cacheImports,
		}, nil)
		require.NoError(t, err)
		dt, err := os.ReadFile(filepath.Join(destDir, "out"))
		require.NoError(t, err)
		return string(dt)
	}

	attrs := map[string]string{"scope": scope, "mounts": "true"}
	maps.Copy(attrs, cacheAttrs)

	// Fill the cache mount with random data and export it.
	written := solve(`head -c 32 /dev/urandom | sha256sum > /cache/data && cp /cache/data out`,
		[]client.CacheOptionsEntry{{Type: "gha", Attrs: attrs}}, nil)

	ensurePruneAll(t, c, sb)
	// Github Cache Service v2 is not immediately consistent.
	time.Sleep(3 * time.Second)

	// A different command misses the layer cache, so it can only see the
	// data if the cache mount was imported.
	read := solve(`cp /cache/data out`, nil, []client.CacheOptionsEntry{{Type: "gha", Attrs: attrs}})
	require.Equal(t, written, read)
}

func ghaCacheAttrs(t *testing.T) map[string]string {
	t.Helper()
	cacheAttrs := map[string]string{}
	if v, ok := os.LookupEnv("ACTIONS_CACHE_SERVICE_V2"); ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			cacheAttrs["url_v2"] = os.Getenv("ACTIONS_RESULTS_URL")
		}
	}
	cacheAttrs["url"] = os.Getenv("ACTIONS_CACHE_URL")
	if cacheAttrs["url"] == "" {
		cacheAttrs["url"] = os.Getenv("ACTIONS_RESULTS_URL")
	}
	cacheAttrs["token"] = os.Getenv("ACTIONS_RUNTIME_TOKEN")
	if cacheAttrs["token"] == "" || (cacheAttrs["url"] == "" && cacheAttrs["url_v2"] == "") {
		t.Skip("actions runtime token and cache url must be set")
	}
	return cacheAttrs
}

func TestResolveCacheMountsScopePrecedence(t *testing.T) {
	indexFor := func(dgst digest.Digest) []byte {
		dt, err := json.Marshal(mountIndex{Mounts: map[string]cacheimporttypes.CacheLayer{
			"shared": {Blob: dgst, Annotations: &cacheimporttypes.LayerAnnotations{
				MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
				DiffID:    digest.FromString("diff-" + dgst.String()),
				Size:      1,
			}},
		}})
		require.NoError(t, err)
		return dt
	}
	mainBlob := digest.FromString("main")
	branchBlob := digest.FromString("branch")
	mainIndex := indexFor(mainBlob)
	var branchIndex []byte
	{
		var idx mountIndex
		require.NoError(t, json.Unmarshal(indexFor(branchBlob), &idx))
		idx.Mounts["branch-only"] = idx.Mounts["shared"]
		dt, err := json.Marshal(idx)
		require.NoError(t, err)
		branchIndex = dt
	}

	// The read-only default branch scope is listed first; the writable
	// branch scope must still win.
	claims, err := json.Marshal(map[string]any{
		"ac":  `[{"Scope":"refs/heads/main","Permission":1},{"Scope":"refs/heads/feature","Permission":3}]`,
		"nbf": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
	cfg, err := getConfig(nil, nil, map[string]string{
		attrToken:      token,
		attrURL:        "https://cache.example",
		attrAPIVersion: "1",
	})
	require.NoError(t, err)

	keys := map[string][]byte{
		mountIndexKey("refs/heads/main", cfg):    mainIndex,
		mountIndexKey("refs/heads/feature", cfg): branchIndex,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_apis/artifactcache/cache":
			key := r.URL.Query().Get("keys")
			if _, ok := keys[key]; !ok {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
				"cacheKey":        key,
				"archiveLocation": "http://" + r.Host + "/blob/" + key,
			}))
		case strings.HasPrefix(r.URL.Path, "/blob/"):
			_, _ = w.Write(keys[strings.TrimPrefix(r.URL.Path, "/blob/")])
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg.URL = server.URL

	ci, err := NewImporter(cfg)
	require.NoError(t, err)
	remotes, err := ci.(*importer).ResolveCacheMounts(t.Context())
	require.NoError(t, err)
	require.Len(t, remotes, 2)
	require.Equal(t, branchBlob, remotes["shared"].Descriptors[0].Digest)
	require.Equal(t, branchBlob, remotes["branch-only"].Descriptors[0].Digest)
	require.Equal(t, digest.FromString("diff-"+branchBlob.String()).String(),
		remotes["shared"].Descriptors[0].Annotations["containerd.io/uncompressed"])
}

func TestCacheMountsRejectSignedCache(t *testing.T) {
	cfg, err := getConfig(&signingconfig.Config{
		Verify: signingconfig.VerifyConfig{Required: true},
	}, nil, map[string]string{
		attrToken: "unused",
		attrURL:   "https://cache.example",
	})
	require.NoError(t, err)
	require.ErrorContains(t, checkMountsSupported(cfg), "not supported with signed")
}

func TestGetConfigMountsOnly(t *testing.T) {
	for mounts, skip := range map[string]bool{"": false, "false": false, "true": false, "only": true} {
		cfg, err := getConfig(nil, nil, map[string]string{
			attrToken: "unused",
			attrURL:   "https://cache.example",
			"mounts":  mounts,
		})
		require.NoError(t, err)
		require.Equal(t, skip, cfg.SkipLayers, "mounts=%q", mounts)
	}
}
