package tsdb // import "github.com/influxdata/influxdb/tsdb"

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/bytesutil"
	"github.com/influxdata/influxdb/pkg/estimator"
	"github.com/influxdata/influxdb/pkg/limiter"
	"github.com/uber-go/zap"
)

var (
	// ErrShardNotFound is returned when trying to get a non existing shard.
	ErrShardNotFound = fmt.Errorf("shard not found")
	// ErrStoreClosed is returned when trying to use a closed Store.
	ErrStoreClosed = fmt.Errorf("store is closed")
)

// Statistics gathered by the store.
const (
	statDatabaseSeries       = "numSeries"       // number of series in a database
	statDatabaseMeasurements = "numMeasurements" // number of measurements in a database
)

// Store manages shards and indexes for databases.
type Store struct {
	mu sync.RWMutex
	// databases keeps track of the number of databases being managed by the store.
	databases map[string]struct{}

	path string

	// shared per-database indexes, only if using "inmem".
	indexes map[string]interface{}

	// shards is a map of shard IDs to the associated Shard.
	shards map[uint64]*Shard

	EngineOptions EngineOptions

	baseLogger zap.Logger
	Logger     zap.Logger

	closing chan struct{}
	wg      sync.WaitGroup
	opened  bool
}

// NewStore returns a new store with the given path and a default configuration.
// The returned store must be initialized by calling Open before using it.
func NewStore(path string) *Store {
	logger := zap.New(zap.NullEncoder())
	return &Store{
		databases:     make(map[string]struct{}),
		path:          path,
		indexes:       make(map[string]interface{}),
		EngineOptions: NewEngineOptions(),
		Logger:        logger,
		baseLogger:    logger,
	}
}

// WithLogger sets the logger for the store.
func (s *Store) WithLogger(log zap.Logger) {
	s.baseLogger = log
	s.Logger = log.With(zap.String("service", "store"))
	for _, sh := range s.shards {
		sh.WithLogger(s.baseLogger)
	}
}

// Statistics returns statistics for period monitoring.
func (s *Store) Statistics(tags map[string]string) []models.Statistic {
	s.mu.RLock()
	shards := s.shardsSlice()
	s.mu.RUnlock()

	// Add all the series and measurements cardinality estimations.
	databases := s.Databases()
	statistics := make([]models.Statistic, 0, len(databases))
	for _, database := range databases {
		sc, err := s.SeriesCardinality(database)
		if err != nil {
			s.Logger.Error("cannot retrieve series cardinality", zap.Error(err))
			continue
		}

		mc, err := s.MeasurementsCardinality(database)
		if err != nil {
			s.Logger.Error("cannot retrieve measurement cardinality", zap.Error(err))
			continue
		}

		statistics = append(statistics, models.Statistic{
			Name: "database",
			Tags: models.StatisticTags{"database": database}.Merge(tags),
			Values: map[string]interface{}{
				statDatabaseSeries:       sc,
				statDatabaseMeasurements: mc,
			},
		})
	}

	// Gather all statistics for all shards.
	for _, shard := range shards {
		statistics = append(statistics, shard.Statistics(tags)...)
	}
	return statistics
}

// Path returns the store's root path.
func (s *Store) Path() string { return s.path }

// Open initializes the store, creating all necessary directories, loading all
// shards as well as initializing periodic maintenance of them.
func (s *Store) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closing = make(chan struct{})
	s.shards = map[uint64]*Shard{}

	s.Logger.Info(fmt.Sprintf("Using data dir: %v", s.Path()))

	// Create directory.
	if err := os.MkdirAll(s.path, 0777); err != nil {
		return err
	}

	if err := s.loadShards(); err != nil {
		return err
	}

	s.opened = true

	return nil
}

func (s *Store) loadShards() error {
	// res holds the result from opening each shard in a goroutine
	type res struct {
		s   *Shard
		err error
	}

	t := limiter.NewFixed(runtime.GOMAXPROCS(0))

	resC := make(chan *res)
	var n int

	// Determine how many shards we need to open by checking the store path.
	dbDirs, err := ioutil.ReadDir(s.path)
	if err != nil {
		return err
	}

	for _, db := range dbDirs {
		if !db.IsDir() {
			s.Logger.Info("Not loading. Not a database directory.", zap.String("name", db.Name()))
			continue
		}

		// Retrieve database index.
		idx, err := s.createIndexIfNotExists(db.Name())
		if err != nil {
			return err
		}

		// Load each retention policy within the database directory.
		rpDirs, err := ioutil.ReadDir(filepath.Join(s.path, db.Name()))
		if err != nil {
			return err
		}

		for _, rp := range rpDirs {
			if !rp.IsDir() {
				s.Logger.Info(fmt.Sprintf("Skipping retention policy dir: %s. Not a directory", rp.Name()))
				continue
			}

			shardDirs, err := ioutil.ReadDir(filepath.Join(s.path, db.Name(), rp.Name()))
			if err != nil {
				return err
			}

			for _, sh := range shardDirs {
				n++
				go func(db, rp, sh string) {
					t.Take()
					defer t.Release()

					start := time.Now()
					path := filepath.Join(s.path, db, rp, sh)
					walPath := filepath.Join(s.EngineOptions.Config.WALDir, db, rp, sh)

					// Shard file names are numeric shardIDs
					shardID, err := strconv.ParseUint(sh, 10, 64)
					if err != nil {
						resC <- &res{err: fmt.Errorf("%s is not a valid ID. Skipping shard.", sh)}
						return
					}

					// Copy options and assign shared index.
					opt := s.EngineOptions
					opt.InmemIndex = idx

					// Existing shards should continue to use inmem index.
					if _, err := os.Stat(filepath.Join(path, "index")); os.IsNotExist(err) {
						opt.IndexVersion = "inmem"
					}

					// Open engine.
					shard := NewShard(shardID, path, walPath, opt)
					shard.WithLogger(s.baseLogger)

					err = shard.Open()
					if err != nil {
						resC <- &res{err: fmt.Errorf("Failed to open shard: %d: %s", shardID, err)}
						return
					}

					resC <- &res{s: shard}
					s.Logger.Info(fmt.Sprintf("%s opened in %s", path, time.Since(start)))
				}(db.Name(), rp.Name(), sh.Name())
			}
		}
	}

	// Gather results of opening shards concurrently, keeping track of how
	// many databases we are managing.
	for i := 0; i < n; i++ {
		res := <-resC
		if res.err != nil {
			s.Logger.Info(res.err.Error())
			continue
		}
		s.shards[res.s.id] = res.s
		s.databases[res.s.database] = struct{}{}
	}
	close(resC)
	return nil
}

// Close closes the store and all associated shards. After calling Close accessing
// shards through the Store will result in ErrStoreClosed being returned.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.opened {
		close(s.closing)
	}
	s.wg.Wait()

	// Close all the shards in parallel.
	if err := s.walkShards(s.shardsSlice(), func(sh *Shard) error {
		return sh.CloseFast()
	}); err != nil {
		return err
	}

	s.opened = false
	s.shards = nil

	return nil
}

// createIndexIfNotExists returns a shared index for a database, if the inmem
// index is being used. If the TSI index is being used, then this method is
// basically a no-op.
func (s *Store) createIndexIfNotExists(name string) (interface{}, error) {
	if idx := s.indexes[name]; idx != nil {
		return idx, nil
	}

	idx, err := NewInmemIndex(name)
	if err != nil {
		return nil, err
	}

	s.indexes[name] = idx
	return idx, nil
}

// Shard returns a shard by id.
func (s *Store) Shard(id uint64) *Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.shards[id]
	if !ok {
		return nil
	}
	return sh
}

// Shards returns a list of shards by id.
func (s *Store) Shards(ids []uint64) []*Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := make([]*Shard, 0, len(ids))
	for _, id := range ids {
		sh, ok := s.shards[id]
		if !ok {
			continue
		}
		a = append(a, sh)
	}
	return a
}

// ShardGroup returns a ShardGroup with a list of shards by id.
func (s *Store) ShardGroup(ids []uint64) ShardGroup {
	return Shards(s.Shards(ids))
}

// ShardN returns the number of shards in the store.
func (s *Store) ShardN() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.shards)
}

// CreateShard creates a shard with the given id and retention policy on a database.
func (s *Store) CreateShard(database, retentionPolicy string, shardID uint64, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.closing:
		return ErrStoreClosed
	default:
	}

	// Shard already exists.
	if _, ok := s.shards[shardID]; ok {
		return nil
	}

	// Create the db and retention policy directories if they don't exist.
	if err := os.MkdirAll(filepath.Join(s.path, database, retentionPolicy), 0700); err != nil {
		return err
	}

	// Create the WAL directory.
	walPath := filepath.Join(s.EngineOptions.Config.WALDir, database, retentionPolicy, fmt.Sprintf("%d", shardID))
	if err := os.MkdirAll(walPath, 0700); err != nil {
		return err
	}

	// Retrieve shared index, if needed.
	idx, err := s.createIndexIfNotExists(database)
	if err != nil {
		return err
	}

	// Copy index options and pass in shared index.
	opt := s.EngineOptions
	opt.InmemIndex = idx

	path := filepath.Join(s.path, database, retentionPolicy, strconv.FormatUint(shardID, 10))
	shard := NewShard(shardID, path, walPath, opt)
	shard.WithLogger(s.baseLogger)
	shard.EnableOnOpen = enabled

	if err := shard.Open(); err != nil {
		return err
	}

	s.shards[shardID] = shard
	s.databases[database] = struct{}{} // Ensure we are tracking any new db.

	return nil
}

// CreateShardSnapShot will create a hard link to the underlying shard and return a path.
// The caller is responsible for cleaning up (removing) the file path returned.
func (s *Store) CreateShardSnapshot(id uint64) (string, error) {
	sh := s.Shard(id)
	if sh == nil {
		return "", ErrShardNotFound
	}

	return sh.CreateSnapshot()
}

// SetShardEnabled enables or disables a shard for read and writes.
func (s *Store) SetShardEnabled(shardID uint64, enabled bool) error {
	sh := s.Shard(shardID)
	if sh == nil {
		return ErrShardNotFound
	}
	sh.SetEnabled(enabled)
	return nil
}

// DeleteShard removes a shard from disk.
func (s *Store) DeleteShard(shardID uint64) error {
	sh := s.Shard(shardID)
	if sh == nil {
		return nil
	}

	// Remove the shard from the database indexes before closing the shard.
	// Closing the shard will do this as well, but it will unload it while
	// the shard is locked which can block stats collection and other calls.
	sh.UnloadIndex()

	if err := sh.Close(); err != nil {
		return err
	}

	if err := os.RemoveAll(sh.path); err != nil {
		return err
	}

	if err := os.RemoveAll(sh.walPath); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.shards, shardID)
	s.mu.Unlock()

	return nil
}

// DeleteDatabase will close all shards associated with a database and remove the directory and files from disk.
func (s *Store) DeleteDatabase(name string) error {
	s.mu.RLock()
	if _, ok := s.databases[name]; !ok {
		s.mu.RUnlock()
		// no files locally, so nothing to do
		return nil
	}
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == name
	})
	s.mu.RUnlock()

	if err := s.walkShards(shards, func(sh *Shard) error {
		if sh.database != name {
			return nil
		}

		return sh.CloseFast()
	}); err != nil {
		return err
	}

	dbPath := filepath.Clean(filepath.Join(s.path, name))

	// extra sanity check to make sure that even if someone named their database "../.."
	// that we don't delete everything because of it, they'll just have extra files forever
	if filepath.Clean(s.path) != filepath.Dir(dbPath) {
		return fmt.Errorf("invalid database directory location for database '%s': %s", name, dbPath)
	}

	if err := os.RemoveAll(dbPath); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.EngineOptions.Config.WALDir, name)); err != nil {
		return err
	}

	s.mu.Lock()
	for _, sh := range shards {
		delete(s.shards, sh.id)
	}

	// Remove database from store list of databases
	delete(s.databases, name)

	// Remove shared index for database if using inmem index.
	delete(s.indexes, name)
	s.mu.Unlock()

	return nil
}

// DeleteRetentionPolicy will close all shards associated with the
// provided retention policy, remove the retention policy directories on
// both the DB and WAL, and remove all shard files from disk.
func (s *Store) DeleteRetentionPolicy(database, name string) error {
	s.mu.RLock()
	if _, ok := s.databases[database]; !ok {
		s.mu.RUnlock()
		// unknown database, nothing to do
		return nil
	}
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == database && sh.retentionPolicy == name
	})
	s.mu.RUnlock()

	// Close and delete all shards under the retention policy on the
	// database.
	if err := s.walkShards(shards, func(sh *Shard) error {
		if sh.database != database || sh.retentionPolicy != name {
			return nil
		}

		return sh.Close()
	}); err != nil {
		return err
	}

	// Remove the retention policy folder.
	rpPath := filepath.Clean(filepath.Join(s.path, database, name))

	// ensure Store's path is the grandparent of the retention policy
	if filepath.Clean(s.path) != filepath.Dir(filepath.Dir(rpPath)) {
		return fmt.Errorf("invalid path for database '%s', retention policy '%s': %s", database, name, rpPath)
	}

	// Remove the retention policy folder.
	if err := os.RemoveAll(filepath.Join(s.path, database, name)); err != nil {
		return err
	}

	// Remove the retention policy folder from the the WAL.
	if err := os.RemoveAll(filepath.Join(s.EngineOptions.Config.WALDir, database, name)); err != nil {
		return err
	}

	s.mu.Lock()
	for _, sh := range shards {
		delete(s.shards, sh.id)
	}
	s.mu.Unlock()
	return nil
}

// DeleteMeasurement removes a measurement and all associated series from a database.
func (s *Store) DeleteMeasurement(database, name string) error {
	s.mu.RLock()
	shards := s.filterShards(byDatabase(database))
	s.mu.RUnlock()

	return s.walkShards(shards, func(sh *Shard) error {
		if err := sh.DeleteMeasurement([]byte(name)); err != nil {
			return err
		}
		return nil
	})
}

// filterShards returns a slice of shards where fn returns true
// for the shard. If the provided predicate is nil then all shards are returned.
func (s *Store) filterShards(fn func(sh *Shard) bool) []*Shard {
	var shards []*Shard
	if fn == nil {
		shards = make([]*Shard, 0, len(s.shards))
		fn = func(*Shard) bool { return true }
	} else {
		shards = make([]*Shard, 0)
	}

	for _, sh := range s.shards {
		if fn(sh) {
			shards = append(shards, sh)
		}
	}
	return shards
}

// byDatabase provides a predicate for filterShards that matches on the name of
// the database passed in.
func byDatabase(name string) func(sh *Shard) bool {
	return func(sh *Shard) bool {
		return sh.database == name
	}
}

// walkShards apply a function to each shard in parallel.  If any of the
// functions return an error, the first error is returned.
func (s *Store) walkShards(shards []*Shard, fn func(sh *Shard) error) error {
	// struct to hold the result of opening each reader in a goroutine
	type res struct {
		err error
	}

	resC := make(chan res)
	var n int

	for _, sh := range shards {
		n++

		go func(sh *Shard) {
			if err := fn(sh); err != nil {
				resC <- res{err: fmt.Errorf("shard %d: %s", sh.id, err)}
				return
			}

			resC <- res{}
		}(sh)
	}

	var err error
	for i := 0; i < n; i++ {
		res := <-resC
		if res.err != nil {
			err = res.err
		}
	}
	close(resC)
	return err
}

// ShardIDs returns a slice of all ShardIDs under management.
func (s *Store) ShardIDs() []uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shardIDs()
}

func (s *Store) shardIDs() []uint64 {
	a := make([]uint64, 0, len(s.shards))
	for shardID := range s.shards {
		a = append(a, shardID)
	}
	return a
}

// shardsSlice returns an ordered list of shards.
func (s *Store) shardsSlice() []*Shard {
	a := make([]*Shard, 0, len(s.shards))
	for _, sh := range s.shards {
		a = append(a, sh)
	}
	sort.Sort(Shards(a))
	return a
}

// Databases returns the names of all databases managed by the store.
func (s *Store) Databases() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	databases := make([]string, 0, len(s.databases))
	for k, _ := range s.databases {
		databases = append(databases, k)
	}
	return databases
}

// DiskSize returns the size of all the shard files in bytes.
// This size does not include the WAL size.
func (s *Store) DiskSize() (int64, error) {
	var size int64

	s.mu.RLock()
	allShards := s.filterShards(nil)
	s.mu.RUnlock()

	for _, sh := range allShards {
		sz, err := sh.DiskSize()
		if err != nil {
			return 0, err
		}
		size += sz
	}
	return size, nil
}

func (s *Store) estimateCardinality(dbName string, getSketches func(*Shard) (estimator.Sketch, estimator.Sketch, error)) (int64, error) {
	var (
		ss estimator.Sketch // Sketch estimating number of items.
		ts estimator.Sketch // Sketch estimating number of tombstoned items.
	)

	s.mu.RLock()
	shards := s.filterShards(byDatabase(dbName))
	s.mu.RUnlock()

	// Iterate over all shards for the database and combine all of the sketches.
	for _, shard := range shards {
		s, t, err := getSketches(shard)
		if err != nil {
			return 0, err
		}

		if ss == nil {
			ss, ts = s, t
		} else if err = ss.Merge(s); err != nil {
			return 0, err
		} else if err = ts.Merge(t); err != nil {
			return 0, err
		}
	}

	if ss != nil {
		return int64(ss.Count() - ts.Count()), nil
	}
	return 0, nil
}

// SeriesCardinality returns the series cardinality for the provided database.
func (s *Store) SeriesCardinality(database string) (int64, error) {
	return s.estimateCardinality(database, func(sh *Shard) (estimator.Sketch, estimator.Sketch, error) {
		if sh == nil {
			return nil, nil, errors.New("shard nil, can't get cardinality")
		}
		return sh.SeriesSketches()
	})
}

// MeasurementsCardinality returns the measurement cardinality for the provided
// database.
func (s *Store) MeasurementsCardinality(database string) (int64, error) {
	return s.estimateCardinality(database, func(sh *Shard) (estimator.Sketch, estimator.Sketch, error) {
		if sh == nil {
			return nil, nil, errors.New("shard nil, can't get cardinality")
		}
		return sh.MeasurementsSketches()
	})
}

// BackupShard will get the shard and have the engine backup since the passed in
// time to the writer.
func (s *Store) BackupShard(id uint64, since time.Time, w io.Writer) error {
	shard := s.Shard(id)
	if shard == nil {
		return fmt.Errorf("shard %d doesn't exist on this server", id)
	}

	path, err := relativePath(s.path, shard.path)
	if err != nil {
		return err
	}

	return shard.engine.Backup(w, path, since)
}

// RestoreShard restores a backup from r to a given shard.
// This will only overwrite files included in the backup.
func (s *Store) RestoreShard(id uint64, r io.Reader) error {
	shard := s.Shard(id)
	if shard == nil {
		return fmt.Errorf("shard %d doesn't exist on this server", id)
	}

	path, err := relativePath(s.path, shard.path)
	if err != nil {
		return err
	}

	return shard.Restore(r, path)
}

// ShardRelativePath will return the relative path to the shard, i.e.,
// <database>/<retention>/<id>.
func (s *Store) ShardRelativePath(id uint64) (string, error) {
	shard := s.Shard(id)
	if shard == nil {
		return "", fmt.Errorf("shard %d doesn't exist on this server", id)
	}
	return relativePath(s.path, shard.path)
}

// DeleteSeries loops through the local shards and deletes the series data for
// the passed in series keys.
func (s *Store) DeleteSeries(database string, sources []influxql.Source, condition influxql.Expr) error {
	// Expand regex expressions in the FROM clause.
	a, err := s.ExpandSources(sources)
	if err != nil {
		return err
	} else if sources != nil && len(sources) != 0 && len(a) == 0 {
		return nil
	}
	sources = a

	// Determine deletion time range.
	min, max, err := influxql.TimeRangeAsEpochNano(condition)
	if err != nil {
		return err
	}

	s.mu.RLock()
	shards := s.filterShards(byDatabase(database))
	s.mu.RUnlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.walkShards(shards, func(sh *Shard) error {
		// Determine list of measurements from sources.
		// Use all measurements if no FROM clause was provided.
		var names []string
		if len(sources) > 0 {
			for _, source := range sources {
				names = append(names, source.(*influxql.Measurement).Name)
			}
		} else {
			if err := sh.engine.ForEachMeasurementName(func(name []byte) error {
				names = append(names, string(name))
				return nil
			}); err != nil {
				return err
			}
		}
		sort.Strings(names)

		// Find matching series keys for each measurement.
		var keys [][]byte
		for _, name := range names {
			a, err := sh.engine.MeasurementSeriesKeysByExpr([]byte(name), condition)
			if err != nil {
				return err
			}
			keys = append(keys, a...)
		}

		if !bytesutil.IsSorted(keys) {
			bytesutil.Sort(keys)
		}

		// Delete all matching keys.
		if err := sh.DeleteSeriesRange(keys, min, max); err != nil {
			return err
		}
		return nil
	})
}

// ExpandSources expands sources against all local shards.
func (s *Store) ExpandSources(sources influxql.Sources) (influxql.Sources, error) {
	shards := func() Shards {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return Shards(s.shardsSlice())
	}()
	return shards.ExpandSources(sources)
}

// WriteToShard writes a list of points to a shard identified by its ID.
func (s *Store) WriteToShard(shardID uint64, points []models.Point) error {
	s.mu.RLock()

	select {
	case <-s.closing:
		s.mu.RUnlock()
		return ErrStoreClosed
	default:
	}

	sh := s.shards[shardID]
	if sh == nil {
		s.mu.RUnlock()
		return ErrShardNotFound
	}
	s.mu.RUnlock()

	return sh.WritePoints(points)
}

// MeasurementNames returns a slice of all measurements. Measurements accepts an
// optional condition expression. If cond is nil, then all measurements for the
// database will be returned.
func (s *Store) MeasurementNames(database string, cond influxql.Expr) ([][]byte, error) {
	s.mu.RLock()
	shards := s.filterShards(byDatabase(database))
	s.mu.RUnlock()

	var names [][]byte
	for _, sh := range shards {
		a, err := sh.MeasurementNamesByExpr(cond)
		if err != nil {
			return nil, err
		}
		names = append(names, a...)
		continue
	}
	bytesutil.Sort(names)

	return names, nil
}

// MeasurementSeriesCounts returns the number of measurements and series in all
// the shards' indices.
func (s *Store) MeasurementSeriesCounts(database string) (measuments int, series int) {
	// TODO: implement me
	return 0, 0
}

type TagValues struct {
	Measurement string
	Values      []KeyValue
}

// TagValues returns the tag keys and values in the given database, matching the condition.
func (s *Store) TagValues(database string, cond influxql.Expr) ([]TagValues, error) {
	if cond == nil {
		return nil, errors.New("a condition is required")
	}

	measurementExpr := influxql.CloneExpr(cond)
	measurementExpr = influxql.Reduce(influxql.RewriteExpr(measurementExpr, func(e influxql.Expr) influxql.Expr {
		switch e := e.(type) {
		case *influxql.BinaryExpr:
			switch e.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
				tag, ok := e.LHS.(*influxql.VarRef)
				if !ok || tag.Val != "_name" {
					return nil
				}
			}
		}
		return e
	}), nil)

	filterExpr := influxql.CloneExpr(cond)
	filterExpr = influxql.Reduce(influxql.RewriteExpr(filterExpr, func(e influxql.Expr) influxql.Expr {
		switch e := e.(type) {
		case *influxql.BinaryExpr:
			switch e.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
				tag, ok := e.LHS.(*influxql.VarRef)
				if !ok || strings.HasPrefix(tag.Val, "_") {
					return nil
				}
			}
		}
		return e
	}), nil)

	// Get all measurements for the shards we're interested in.
	s.mu.RLock()
	shards := s.filterShards(byDatabase(database))
	s.mu.RUnlock()

	var tagValues []TagValues
	for _, sh := range shards {
		names, err := sh.MeasurementNamesByExpr(measurementExpr)
		if err != nil {
			return nil, err
		}

		for _, name := range names {
			// Determine a list of keys from condition.
			keySet, err := sh.engine.MeasurementTagKeysByExpr(name, cond)
			if err != nil {
				return nil, err
			}

			// Loop over all keys for each series.
			m := make(map[KeyValue]struct{})
			if err := sh.engine.ForEachMeasurementSeriesByExpr(name, filterExpr, func(tags models.Tags) error {
				for _, t := range tags {
					if _, ok := keySet[string(t.Key)]; ok {
						m[KeyValue{string(t.Key), string(t.Value)}] = struct{}{}
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}

			/*
				// Loop over all keys for each series.
				m := make(map[KeyValue]struct{}, len(ss))
				for _, series := range ss {
					series.ForEachTag(func(t models.Tag) {
						if !ok {
							// nop
						} else if _, exists := keySet[string(t.Key)]; !exists {
							return
						}
						m[KeyValue{string(t.Key), string(t.Value)}] = struct{}{}
					})
				}
			*/

			// Sort key/value set.
			var a []KeyValue
			if len(m) > 0 {
				a = make([]KeyValue, 0, len(m))
				for kv := range m {
					a = append(a, kv)
				}
				sort.Sort(KeyValues(a))
			}

			tagValues = append(tagValues, TagValues{
				Measurement: string(name),
				Values:      a,
			})
		}
	}

	return tagValues, nil
}

// KeyValue holds a string key and a string value.
type KeyValue struct {
	Key, Value string
}

// KeyValues is a sortable slice of KeyValue.
type KeyValues []KeyValue

// Len implements sort.Interface.
func (a KeyValues) Len() int { return len(a) }

// Swap implements sort.Interface.
func (a KeyValues) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Less implements sort.Interface. Keys are compared before values.
func (a KeyValues) Less(i, j int) bool {
	ki, kj := a[i].Key, a[j].Key
	if ki == kj {
		return a[i].Value < a[j].Value
	}
	return ki < kj
}

// filterShowSeriesResult will limit the number of series returned based on the limit and the offset.
// Unlike limit and offset on SELECT statements, the limit and offset don't apply to the number of Rows, but
// to the number of total Values returned, since each Value represents a unique series.
func (e *Store) filterShowSeriesResult(limit, offset int, rows models.Rows) models.Rows {
	var filteredSeries models.Rows
	seriesCount := 0
	for _, r := range rows {
		var currentSeries [][]interface{}

		// filter the values
		for _, v := range r.Values {
			if seriesCount >= offset && seriesCount-offset < limit {
				currentSeries = append(currentSeries, v)
			}
			seriesCount++
		}

		// only add the row back in if there are some values in it
		if len(currentSeries) > 0 {
			r.Values = currentSeries
			filteredSeries = append(filteredSeries, r)
			if seriesCount > limit+offset {
				return filteredSeries
			}
		}
	}
	return filteredSeries
}

// decodeStorePath extracts the database and retention policy names
// from a given shard or WAL path.
func decodeStorePath(shardOrWALPath string) (database, retentionPolicy string) {
	// shardOrWALPath format: /maybe/absolute/base/then/:database/:retentionPolicy/:nameOfShardOrWAL

	// Discard the last part of the path (the shard name or the wal name).
	path, _ := filepath.Split(filepath.Clean(shardOrWALPath))

	// Extract the database and retention policy.
	path, rp := filepath.Split(filepath.Clean(path))
	_, db := filepath.Split(filepath.Clean(path))
	return db, rp
}

// relativePath will expand out the full paths passed in and return
// the relative shard path from the store
func relativePath(storePath, shardPath string) (string, error) {
	path, err := filepath.Abs(storePath)
	if err != nil {
		return "", fmt.Errorf("store abs path: %s", err)
	}

	fp, err := filepath.Abs(shardPath)
	if err != nil {
		return "", fmt.Errorf("file abs path: %s", err)
	}

	name, err := filepath.Rel(path, fp)
	if err != nil {
		return "", fmt.Errorf("file rel path: %s", err)
	}

	return name, nil
}