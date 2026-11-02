package scom

import (
	"context"
	"slices"
)

// InstanceScope returns the instances a query is about: the explicit
// selection if there is one, otherwise every instance in the class and/or
// group (see ResolveInstanceIDs), cached for scopeCacheTTL. It returns nil
// when nothing is chosen at all.
func (db *DB) InstanceScope(ctx context.Context, classID, groupID string, instanceIDs []string) ([]string, error) {
	if len(instanceIDs) > 0 {
		return instanceIDs, nil
	}
	if classID == "" && groupID == "" {
		return nil, nil
	}
	return db.cache().ids.get(ctx, cacheKey("resolve", scopeKey(classID, groupID, nil)), scopeCacheTTL,
		func(ctx context.Context) ([]string, error) {
			return ResolveInstanceIDs(ctx, db.Operational, classID, groupID)
		})
}

// ExpandedScope is InstanceScope widened to everything those instances host
// or contain. Performance counters and alerts usually live on the hosted
// objects (disks, databases, services), not on the instance itself. A
// class/group scope resolves and expands in one batch (ExpandScopeIDs).
// Cached for scopeCacheTTL. It returns nil when nothing is chosen at all.
func (db *DB) ExpandedScope(ctx context.Context, classID, groupID string, instanceIDs []string) ([]string, error) {
	if len(instanceIDs) > 0 {
		return db.cache().ids.get(ctx, cacheKey("expand", idSetKey(instanceIDs)), scopeCacheTTL,
			func(ctx context.Context) ([]string, error) {
				return ExpandHostedEntityIDs(ctx, db.Operational, instanceIDs)
			})
	}
	if classID == "" && groupID == "" {
		return nil, nil
	}
	return db.cache().ids.get(ctx, cacheKey("expandScope", scopeKey(classID, groupID, nil)), scopeCacheTTL,
		func(ctx context.Context) ([]string, error) {
			return ExpandScopeIDs(ctx, db.Operational, classID, groupID)
		})
}

// InstanceOptions lists a class/group's instances (up to
// variableValuesLimit) for a dashboard variable, cached for scopeCacheTTL.
func (db *DB) InstanceOptions(ctx context.Context, classID, groupID string) ([]Option, error) {
	return db.cache().options.get(ctx, cacheKey("instances", scopeKey(classID, groupID, nil)), scopeCacheTTL,
		func(ctx context.Context) ([]Option, error) {
			return ListInstances(ctx, db.Operational, classID, groupID)
		})
}

// CounterScope is what the counter pickers search within: a sample of the
// selection, hosting-expanded (see CounterPickerScope). Key identifies the
// selection for caching picker results. Scoped is false when nothing was
// chosen, in which case the pickers search every counter.
type CounterScope struct {
	IDs    []string
	Key    string
	Scoped bool
}

// CounterPickerScope returns the counter pickers' scope. A broad selection
// (a whole class or group, or more than CounterPickerSampleSize explicit
// instances) is represented by its first few instances: instances of one
// class report near-identical counters, so a sample finds the same options
// at a fraction of the cost. The class/group sample is taken in SQL, sorted
// by name, so the same scope always samples the same instances.
func (db *DB) CounterPickerScope(ctx context.Context, classID, groupID string, instanceIDs []string) (CounterScope, error) {
	key := scopeKey(classID, groupID, instanceIDs)
	if len(instanceIDs) == 0 && classID == "" && groupID == "" {
		return CounterScope{Key: key}, nil
	}
	ids, err := db.cache().ids.get(ctx, cacheKey("picker", key), scopeCacheTTL, func(ctx context.Context) ([]string, error) {
		seed := slices.Clone(instanceIDs)
		slices.Sort(seed)
		seed = SampleCounterScopeIDs(slices.Compact(seed))
		if len(seed) == 0 {
			sampled, err := sampleInstanceIDs(ctx, db.Operational, classID, groupID, CounterPickerSampleSize)
			if err != nil {
				return nil, err
			}
			seed = sampled
		}
		if len(seed) == 0 {
			return []string{}, nil
		}
		return ExpandHostedEntityIDs(ctx, db.Operational, seed)
	})
	if err != nil {
		return CounterScope{}, err
	}
	return CounterScope{IDs: ids, Key: key, Scoped: true}, nil
}
