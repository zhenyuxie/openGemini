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

package immutable

import (
	"fmt"
	"io"

	"github.com/openGemini/openGemini/engine/immutable/readcache"
	"github.com/openGemini/openGemini/lib/bufferpool"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/fileops"
	"go.uber.org/zap"
)

var mmapEn = !config.Is32BitPtr
var readCacheEn = false

func EnableMmapRead(en bool) {
	mmapEn = en
}

func EnableReadCache(readCacheLimit int) {
	if readCacheLimit > 0 {
		readCacheEn = true
		readcache.SetCacheLimitSize(readCacheLimit)
	} else {
		readCacheEn = false
	}
}

type DiskFileReader interface {
	Name() string
	ReadAt(off int64, size uint32, dst *[]byte) ([]byte, error)
	Rename(newName string) error
	IsMmapRead() bool
	Close() error
}

type diskFileReader struct {
	fd       fileops.File
	fileSize int64
	mmapData []byte
}

func NewDiskFileReader(f fileops.File) *diskFileReader {
	fName := f.Name()
	fi, err := f.Stat()
	if err != nil {
		log.Error("stat file fail", zap.String("file", fName), zap.Error(err))
		panic(err)
	}

	fileSize := fi.Size()
	r := &diskFileReader{fd: f, fileSize: fileSize}
	if mmapEn {
		r.mmapData, err = fileops.Mmap(f.Fd(), int(fileSize))
		if err != nil {
			err = errMapFail(fName, err)
			log.Error("mmap file fail", zap.Error(err))
		}
	}

	return r
}

func (r *diskFileReader) IsMmapRead() bool {
	return r.mmapData != nil
}

func (r *diskFileReader) ReadAt(off int64, size uint32, dstPtr *[]byte) ([]byte, error) {
	if size < 1 {
		return nil, nil
	}

	if off < 0 || off > r.fileSize {
		err := fmt.Errorf("invalid read offset %v, filesize %v", off, r.fileSize)
		err = errReadFail(r.Name(), err)
		log.Error(err.Error())
		return nil, err
	}

	if len(r.mmapData) > 0 {
		if off > int64(len(r.mmapData)) {
			err := fmt.Errorf("off=%d, size=%v is out of allowed len=%d", off, size, len(r.mmapData))
			err = errReadFail(r.Name(), err)
			log.Error(err.Error())
			return nil, err
		}

		end := off + int64(size)
		rb := r.mmapData[off:end]

		if dstPtr != nil && len(*dstPtr) > 0 {
			*dstPtr = bufferpool.Resize(*dstPtr, int(size))
			n := copy(*dstPtr, rb)
			return (*dstPtr)[:n], nil
		}

		return rb, nil
	}

	*dstPtr = bufferpool.Resize(*dstPtr, int(size))
	dst := *dstPtr

	n, err := r.fd.ReadAt(dst, off)
	if err != nil {
		if err == io.EOF {
			return dst[:n], nil
		}
		err = errReadFail(r.Name(), err)
		log.Error(err.Error())
		return nil, err
	}

	return dst[:n], nil
}

func (r *diskFileReader) Name() string {
	return r.fd.Name()
}

func (r *diskFileReader) Rename(newName string) error {
	if r.mmapData != nil {
		_ = fileops.MUnmap(r.mmapData)
		r.mmapData = nil
	}

	oldName := r.fd.Name()
	if err := r.fd.Close(); err != nil {
		log.Error("close file fail", zap.String("file", oldName), zap.Error(err))
		err = errCloseFail(oldName, err)
		return err
	}
	log.Debug("rename file", zap.String("old", oldName), zap.String("new", newName), zap.Int64("size", r.fileSize))
	lock := fileops.FileLockOption("")
	if err := fileops.RenameFile(oldName, newName, lock); err != nil {
		err = errRenameFail(zap.String("old", oldName), zap.String("new", newName), err)
		log.Error("rename file fail", zap.Error(err))
		return err
	}

	var err error
	pri := fileops.FilePriorityOption(fileops.IO_PRIORITY_NORMAL)
	r.fd, err = fileops.Open(newName, lock, pri)
	if err != nil {
		err = errOpenFail(newName, err)
		log.Error("open file fail", zap.Error(err))
		return err
	}

	if mmapEn {
		r.mmapData, err = fileops.Mmap(r.fd.Fd(), int(r.fileSize))
		if err != nil {
			err = errMapFail(newName, err)
			log.Error("mmap file fail", zap.Error(err))
		}
	}

	return nil
}

func (r *diskFileReader) Close() error {
	name := r.fd.Name()
	if r.mmapData != nil {
		if err := fileops.MUnmap(r.mmapData); err != nil {
			log.Error("munmap file fail", zap.String("name", name), zap.Error(err))
			return err
		}
	}
	if readCacheEn {
		cacheIns := readcache.GetReadCacheIns()
		cacheIns.Remove(r.Name())
	}

	return r.fd.Close()
}
