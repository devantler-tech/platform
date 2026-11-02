package backupcatalogue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/logging"
)

type factoryOptions struct {
	Handle string `json:"handle"`
}

var catalogueFactories = struct {
	once   sync.Once
	mu     sync.Mutex
	stores map[string]*readStore
}{stores: make(map[string]*readStore)}

func registerStore(s *readStore) (string, func(), error) {
	catalogueFactories.once.Do(func() {
		blob.AddSupportedStorage("catalogue-ro", factoryOptions{}, func(ctx context.Context, options *factoryOptions, create bool) (blob.Storage, error) {
			catalogueFactories.mu.Lock()
			store := catalogueFactories.stores[options.Handle]
			catalogueFactories.mu.Unlock()
			if store == nil {
				return nil, errReadHeld
			}
			if create || !contextActive(ctx) {
				return nil, store.guard.hold()
			}
			return store, nil
		})
	})
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, errReadHeld
	}
	handle := hex.EncodeToString(random[:])
	catalogueFactories.mu.Lock()
	catalogueFactories.stores[handle] = s
	catalogueFactories.mu.Unlock()
	return handle, func() {
		catalogueFactories.mu.Lock()
		delete(catalogueFactories.stores, handle)
		catalogueFactories.mu.Unlock()
	}, nil
}

// observeRepository writes only a credential-free temporary config, inside an
// owned private directory. Backend credentials and the repository password stay
// in memory. Every error discards the entire receipt.
func observeRepository(ctx context.Context, p Policy, accessKey, secretKey, password, parent string, base http.RoundTripper) (result Catalogue, err error) {
	if !safeEnvironment() || password == "" || !contextActive(ctx) {
		return Catalogue{}, errReadHeld
	}
	ctx = logging.WithLogger(ctx, nil)
	store, err := newStorage(ctx, p, accessKey, secretKey, base)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	defer store.guard.Close()
	handle, unregister, err := registerStore(store)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	defer unregister()
	directory, err := os.MkdirTemp(parent, "backup-catalogue-")
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	defer func() {
		if os.RemoveAll(directory) != nil {
			result = Catalogue{}
			err = errReadHeld
		}
	}()
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return Catalogue{}, errReadHeld
	}
	config := repo.LocalConfig{Storage: &blob.ConnectionInfo{Type: "catalogue-ro", Config: &factoryOptions{Handle: handle}}, Caching: nil,
		ClientOptions: repo.ClientOptions{Hostname: "catalogue-reader", Username: "catalogue-reader", ReadOnly: true, EnableActions: false}}
	data, err := json.Marshal(config)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	configPath := filepath.Join(directory, "repository.config")
	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return Catalogue{}, errReadHeld
	}
	r, err := repo.Open(ctx, configPath, password, &repo.Options{DisableRepositoryLog: true, DoNotWaitForUpgrade: true, TraceStorage: false, OnFatalError: func(error) { _ = store.guard.hold() }})
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	defer func() {
		if r.Close(ctx) != nil || store.guard.Err() != nil || !contextActive(ctx) {
			result = Catalogue{}
			err = errReadHeld
		}
	}()
	options := r.ClientOptions()
	if !options.ReadOnly || options.EnableActions || options.PermissiveCacheLoading {
		return Catalogue{}, errReadHeld
	}
	direct, ok := r.(repo.DirectRepository)
	if !ok || len(direct.UniqueID()) == 0 {
		return Catalogue{}, errReadHeld
	}
	result, err = readCatalogue(ctx, r)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	bound, err := json.Marshal(struct {
		Endpoint, Bucket, Prefix string
		Repository               []byte
	}{p.Endpoint, p.Bucket, p.Prefix, direct.UniqueID()})
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	digest := sha256.Sum256(bound)
	result.RepositoryDigest = hex.EncodeToString(digest[:])
	return result, nil
}
