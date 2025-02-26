/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tsi

import (
	"time"
	"unsafe"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/workingsetcache"
	"github.com/VictoriaMetrics/fastcache"
)

const (
	SeriesKeyToTSIDCacheName = "seriesKey_tsid"
	TSIDToSeriesKeyCacheName = "tsid_seriesKey"
)

type IndexCache struct {
	// series key -> TSID.
	SeriesKeyToTSIDCache *workingsetcache.Cache

	// TSID -> series key
	TSIDToSeriesKeyCache *workingsetcache.Cache

	// Cache for fast TagFilters -> TSIDs lookup.
	tagCache *workingsetcache.Cache

	metrics *IndexMetrics

	path string
}

type IndexMetrics struct {
	TSIDCacheSize      uint64
	TSIDCacheSizeBytes uint64
	TSIDCacheRequests  uint64
	TSIDCacheMisses    uint64

	SKeyCacheSize      uint64
	SKeyCacheSizeBytes uint64
	SKeyCacheRequests  uint64
	SKeyCacheMisses    uint64

	TagCacheSize      uint64
	TagCacheSizeBytes uint64
	TagCacheRequests  uint64
	TagCacheMisses    uint64
}

func (ic *IndexCache) GetTSIDFromTSIDCache(id *uint64, key []byte) bool {
	if ic.SeriesKeyToTSIDCache == nil {
		return false
	}
	buf := (*[unsafe.Sizeof(*id)]byte)(unsafe.Pointer(id))[:]
	buf = ic.SeriesKeyToTSIDCache.Get(buf[:0], key)
	return uintptr(len(buf)) == unsafe.Sizeof(*id)
}

func (ic *IndexCache) PutTSIDToTSIDCache(id *uint64, key []byte) {
	buf := (*[unsafe.Sizeof(*id)]byte)(unsafe.Pointer(id))[:]
	ic.SeriesKeyToTSIDCache.Set(key, buf)
}

func (ic *IndexCache) putToSeriesKeyCache(id uint64, seriesKey []byte) {
	key := (*[unsafe.Sizeof(id)]byte)(unsafe.Pointer(&id))
	ic.TSIDToSeriesKeyCache.Set(key[:], seriesKey)
}

func (ic *IndexCache) getFromSeriesKeyCache(dst []byte, id uint64) []byte {
	key := (*[unsafe.Sizeof(id)]byte)(unsafe.Pointer(&id))
	return ic.TSIDToSeriesKeyCache.Get(dst, key[:])
}

func (ic *IndexCache) close() error {
	if err := ic.SeriesKeyToTSIDCache.Save(ic.path + "/" + SeriesKeyToTSIDCacheName); err != nil {
		return err
	}
	ic.SeriesKeyToTSIDCache.Stop()

	if err := ic.TSIDToSeriesKeyCache.Save(ic.path + "/" + TSIDToSeriesKeyCacheName); err != nil {
		return err
	}
	ic.TSIDToSeriesKeyCache.Stop()

	ic.tagCache.Stop()

	return nil
}

func (ic *IndexCache) UpdateMetrics() {
	var cs fastcache.Stats

	cs.Reset()
	ic.SeriesKeyToTSIDCache.UpdateStats(&cs)
	ic.metrics.TSIDCacheSize += cs.EntriesCount
	ic.metrics.TSIDCacheSizeBytes += cs.BytesSize
	ic.metrics.TSIDCacheRequests += cs.GetCalls
	ic.metrics.TSIDCacheMisses += cs.Misses

	cs.Reset()
	ic.TSIDToSeriesKeyCache.UpdateStats(&cs)
	ic.metrics.SKeyCacheSize += cs.EntriesCount
	ic.metrics.SKeyCacheSizeBytes += cs.BytesSize
	ic.metrics.SKeyCacheRequests += cs.GetCalls
	ic.metrics.SKeyCacheMisses += cs.Misses

	cs.Reset()
	ic.tagCache.UpdateStats(&cs)
	ic.metrics.TagCacheSize += cs.EntriesCount
	ic.metrics.TagCacheSizeBytes += cs.BytesSize
	ic.metrics.TagCacheRequests += cs.GetBigCalls
	ic.metrics.TagCacheMisses += cs.Misses
}

func LoadCache(info, name, cachePath string, sizeBytes int) *workingsetcache.Cache {
	path := cachePath + "/" + name
	c := workingsetcache.Load(path, sizeBytes, time.Hour)
	var cs fastcache.Stats
	c.UpdateStats(&cs)
	return c
}

func NewIndexCache(tsidCacheSize, skeyCacheSize, tagCacheSize int, path string) *IndexCache {
	if tsidCacheSize == 0 {
		tsidCacheSize = defaultTSIDCacheSize
	}
	if skeyCacheSize == 0 {
		skeyCacheSize = defaultSKeyCacheSize
	}
	if tagCacheSize == 0 {
		tagCacheSize = defaultTagCacheSize
	}
	ic := &IndexCache{
		SeriesKeyToTSIDCache: LoadCache("SeriesKey->TSID", SeriesKeyToTSIDCacheName, path, tsidCacheSize),
		TSIDToSeriesKeyCache: LoadCache("TSID->SeriesKey", TSIDToSeriesKeyCacheName, path, skeyCacheSize),
		tagCache:             workingsetcache.New(tagCacheSize, time.Hour),
		path:                 path,
	}
	return ic
}
