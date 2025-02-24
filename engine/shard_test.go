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

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openGemini/openGemini/engine/comm"
	"github.com/openGemini/openGemini/engine/executor"
	"github.com/openGemini/openGemini/engine/hybridqp"
	"github.com/openGemini/openGemini/engine/immutable"
	"github.com/openGemini/openGemini/engine/index/tsi"
	"github.com/openGemini/openGemini/lib/bufferpool"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/rand"
	"github.com/openGemini/openGemini/lib/record"
	"github.com/openGemini/openGemini/open_src/influx/influxql"
	"github.com/openGemini/openGemini/open_src/influx/meta"
	"github.com/openGemini/openGemini/open_src/influx/query"
	"github.com/openGemini/openGemini/open_src/vm/protoparser/influx"
)

func init() {
	immutable.EnableMergeOutOfOrder = false
	logger.InitLogger(config.NewLogger(config.AppStore))
	immutable.Init()
}

const defaultDb = "db0"
const defaultRp = "rp0"
const defaultShGroupId = uint64(1)
const defaultShardId = uint64(1)
const defaultPtId = uint32(1)
const defaultChunkSize = 1000
const defaultMeasurementName = "cpu"

type checkSingleCursorFunction func(cur comm.KeyCursor, expectRecords *sync.Map, total *int, ascending bool, stop chan struct{}) error
type checkAggSingleCursorFunction func(cur comm.KeyCursor, expectRecords *sync.Map, stop chan struct{}) error

func GenDataRecord(msNames []string, seriesNum, pointNumOfPerSeries int, interval time.Duration,
	tm time.Time, fullField, inc bool, fixBool bool, tv ...int) ([]influx.Row, int64, int64) {
	tm = tm.Truncate(time.Second)
	pts := make([]influx.Row, 0, seriesNum)
	names := msNames
	if len(msNames) == 0 {
		names = []string{defaultMeasurementName}
	}

	mms := func(i int) string {
		return names[i%len(names)]
	}

	var indexKeyPool []byte
	vInt, vFloat := int64(1), 1.1
	tv1, tv2, tv3, tv4 := 1, 1, 1, 1
	for _, tgv := range tv {
		tv1 = tgv
	}
	for i := 0; i < seriesNum; i++ {
		fields := map[string]interface{}{
			"field2_int":    vInt,
			"field3_bool":   i%2 == 0,
			"field4_float":  vFloat,
			"field1_string": fmt.Sprintf("test-test-test-test-%d", i),
		}
		if fixBool {
			fields["field3_bool"] = (i%2 == 0)
		} else {
			fields["field3_bool"] = (rand.Int31n(100) > 50)
		}

		if !fullField {
			if i%10 == 0 {
				delete(fields, "field1_string")
			}

			if i%25 == 0 {
				delete(fields, "field4_float")
			}

			if i%35 == 0 {
				delete(fields, "field3_bool")
			}
		}

		r := influx.Row{}

		// fields init
		r.Fields = make([]influx.Field, len(fields))
		j := 0
		for k, v := range fields {
			r.Fields[j].Key = k
			switch v.(type) {
			case int64:
				r.Fields[j].Type = influx.Field_Type_Int
				r.Fields[j].NumValue = float64(v.(int64))
			case float64:
				r.Fields[j].Type = influx.Field_Type_Float
				r.Fields[j].NumValue = v.(float64)
			case string:
				r.Fields[j].Type = influx.Field_Type_String
				r.Fields[j].StrValue = v.(string)
			case bool:
				r.Fields[j].Type = influx.Field_Type_Boolean
				if v.(bool) == false {
					r.Fields[j].NumValue = 0
				} else {
					r.Fields[j].NumValue = 1
				}
			}
			j++
		}

		sort.Sort(r.Fields)

		vInt++
		vFloat += 1.1
		tags := map[string]string{
			"tagkey1": fmt.Sprintf("tagvalue1_%d", tv1),
			"tagkey2": fmt.Sprintf("tagvalue2_%d", tv2),
			"tagkey3": fmt.Sprintf("tagvalue3_%d", tv3),
			"tagkey4": fmt.Sprintf("tagvalue4_%d", tv4),
		}

		// tags init
		r.Tags = make(influx.PointTags, len(tags))
		j = 0
		for k, v := range tags {
			r.Tags[j].Key = k
			r.Tags[j].Value = v
			j++
		}
		sort.Sort(&r.Tags)
		tv4++
		tv3++
		tv2++
		tv1++

		name := mms(i)
		r.Name = name
		r.Timestamp = tm.UnixNano()
		r.UnmarshalIndexKeys(indexKeyPool)
		r.ShardKey = r.IndexKey
		tm = tm.Add(interval)

		pts = append(pts, r)
	}
	if pointNumOfPerSeries > 1 {
		copyRs := copyPointRows(pts, pointNumOfPerSeries-1, interval, inc)
		pts = append(pts, copyRs...)
	}

	sort.Slice(pts, func(i, j int) bool {
		return pts[i].Timestamp < pts[j].Timestamp
	})

	return pts, pts[0].Timestamp, pts[len(pts)-1].Timestamp
}

// create field aux, if input slice is nil, means create for all default fields
func createFieldAux(fieldsName []string) []influxql.VarRef {
	var fieldAux []influxql.VarRef
	fieldMap := make(map[string]influxql.DataType, 4)
	fieldMap["field1_string"] = influxql.String
	fieldMap["field2_int"] = influxql.Integer
	fieldMap["field3_bool"] = influxql.Boolean
	fieldMap["field4_float"] = influxql.Float

	if len(fieldsName) == 0 {
		fieldsName = append(fieldsName, "field1_string", "field2_int", "field3_bool", "field4_float")
	}
	for _, fieldName := range fieldsName {
		if tp, ok := fieldMap[fieldName]; ok {
			fieldAux = append(fieldAux, influxql.VarRef{Val: fieldName, Type: tp})
		}
	}

	return fieldAux
}

func createShard(db, rp string, ptId uint32, pathName string) (*shard, error) {
	indexPath := filepath.Join(pathName, defaultDb, "/index/data")
	ident := &meta.IndexIdentifier{OwnerDb: db, OwnerPt: ptId, Policy: rp}
	opts := new(tsi.Options).
		Ident(ident).
		Path(indexPath).
		IndexType(tsi.MergeSet).
		EndTime(time.Now().Add(time.Hour)).
		Duration(time.Hour)
	indexBuilder := tsi.NewIndexBuilder(opts)
	indexBuilder.Relations = make(map[uint32]*tsi.IndexRelation)
	primaryIndex, err := tsi.NewIndex(opts)
	if err != nil {
		return nil, err
	}
	primaryIndex.SetIndexBuilder(indexBuilder)
	indexRelation, err := tsi.NewIndexRelation(opts, primaryIndex, indexBuilder)
	indexBuilder.Relations[uint32(tsi.MergeSet)] = indexRelation
	err = indexBuilder.Open()
	if err != nil {
		return nil, err
	}
	dataPath := pathName + "/data"
	walPath := pathName + "/wal"
	shardDuration := &meta.DurationDescriptor{Tier: meta.Hot, TierDuration: time.Hour}
	tr := &meta.TimeRangeInfo{StartTime: mustParseTime(time.RFC3339Nano, "1970-01-01T01:00:00Z"),
		EndTime: mustParseTime(time.RFC3339Nano, "2099-01-01T01:00:00Z")}
	shardIdent := &meta.ShardIdentifier{ShardID: defaultShardId, ShardGroupID: 1, OwnerDb: db, OwnerPt: ptId, Policy: rp}
	sh := NewShard(dataPath, walPath, shardIdent, indexBuilder, shardDuration, tr, defaultEngineOption)

	if err := sh.Open(); err != nil {
		_ = sh.Close()
		return nil, err
	}
	return sh, nil
}

func closeShard(sh *shard) error {
	if err := sh.indexBuilder.Close(); err != nil {
		return err
	}
	if err := sh.Close(); err != nil {
		return err
	}
	return nil
}

func writeData(sh *shard, rs []influx.Row, forceFlush bool) error {
	var buff []byte
	var err error
	buff, err = influx.FastMarshalMultiRows(buff, rs)
	if err != nil {
		return err
	}

	for i := range rs {
		sort.Sort(rs[i].Fields)
	}

	err = sh.WriteRows(rs, buff)
	if err != nil {
		return err
	}

	// wait index flush
	//time.Sleep(time.Second * 1)
	if forceFlush {
		// wait mem table flush
		sh.ForceFlush()
	}
	return nil
}

func genQuerySchema(fieldAux []influxql.VarRef, opt *query.ProcessorOptions) *executor.QuerySchema {
	var fields influxql.Fields
	var columnNames []string
	for i := range fieldAux {
		f := &influxql.Field{
			Expr:  &fieldAux[i],
			Alias: "",
		}
		fields = append(fields, f)
		columnNames = append(columnNames, fieldAux[i].Val)
	}
	return executor.NewQuerySchema(fields, columnNames, opt)
}

func genQueryOpt(tc *TestCase, msName string, ascending bool) *query.ProcessorOptions {
	var opt query.ProcessorOptions
	opt.Name = msName
	opt.Dimensions = tc.dims
	opt.Ascending = ascending
	opt.FieldAux = tc.fieldAux
	opt.MaxParallel = 8
	opt.ChunkSize = defaultChunkSize
	opt.StartTime = tc.startTime
	opt.EndTime = tc.endTime

	addFilterFieldCondition(tc.fieldFilter, &opt)

	return &opt
}

func appendFieldValueToRecord(rec *record.Record, fields []influx.Field, timeStamp int64) {
	fieldExist := false
	for _, v := range fields {
		recIndex := rec.FieldIndexs(v.Key)
		if recIndex == -1 {
			continue
		}
		fieldExist = true
		if v.Type == influx.Field_Type_Int {
			rec.ColVals[recIndex].AppendInteger(int64(v.NumValue))
		} else if v.Type == influx.Field_Type_Float {
			rec.ColVals[recIndex].AppendFloat(v.NumValue)
		} else if v.Type == influx.Field_Type_Boolean {
			if 0 == v.NumValue {
				rec.ColVals[recIndex].AppendBoolean(false)
			} else {
				rec.ColVals[recIndex].AppendBoolean(true)
			}
		} else if v.Type == influx.Field_Type_String {
			rec.ColVals[recIndex].AppendString(v.StrValue)
		} else {
			panic("error type")
		}
	}
	if fieldExist {
		rec.ColVals[len(rec.Schema)-1].AppendInteger(timeStamp)
	}
}

func transRowToRecordNew(row *influx.Row, schema record.Schemas) *record.Record {
	var rec record.Record
	rec.Schema = schema
	rec.ColVals = make([]record.ColVal, len(schema))

	appendFieldValueToRecord(&rec, row.Fields, row.Timestamp)

	sort.Sort(rec)
	return &rec
}

func copyPointRows(rs []influx.Row, copyCnt int, interval time.Duration, inc bool) []influx.Row {
	copyRs := make([]influx.Row, 0, copyCnt*len(rs))
	for i := 0; i < copyCnt; i++ {
		cnt := int64(i + 1)
		for index, endIndex := 0, len(rs); index < endIndex; index++ {
			tmpPointRow := rs[index]
			tmpPointRow.Timestamp += int64(interval) * cnt
			// fields need regenerate
			if inc {
				tmpPointRow.Fields = make([]influx.Field, len(rs[index].Fields))
				if len(rs[index].Fields) > 0 {
					tmpPointRow.Copy(&rs[index])
					for idx, field := range rs[index].Fields {
						switch field.Type {
						case influx.Field_Type_Int:
							tmpPointRow.Fields[idx].NumValue += float64(cnt)
						case influx.Field_Type_Float:
							tmpPointRow.Fields[idx].NumValue += float64(cnt)
						case influx.Field_Type_Boolean:
							tmpPointRow.Fields[idx].NumValue = float64(cnt & 1)
						case influx.Field_Type_String:
							tmpPointRow.Fields[idx].StrValue += fmt.Sprintf("-%d", cnt)
						}
					}
				}
			}

			copyRs = append(copyRs, tmpPointRow)
		}
	}
	return copyRs
}

func copyIntervalStepPointRows(rs []influx.Row, copyCnt int, interval time.Duration, inc bool) []influx.Row {
	copyRs := make([]influx.Row, 0, copyCnt*len(rs))
	for i := 0; i < copyCnt; i++ {
		for index, endIndex := 0, len(rs); index < endIndex; index++ {
			tmpPointRow := rs[index]
			tmpPointRow.Timestamp += int64(interval)
			// fields need regenerate
			if inc {
				tmpPointRow.Fields = make([]influx.Field, len(rs[index].Fields))
				if len(rs[index].Fields) > 0 {
					tmpPointRow.Copy(&rs[index])
				}
			}

			copyRs = append(copyRs, tmpPointRow)
		}
	}
	return copyRs
}

type seriesData struct {
	rec  *record.Record   // record of this series
	tags influx.PointTags // used for filter and make group key later
}

func genExpectRecordsMap(rs []influx.Row, querySchema *executor.QuerySchema) *sync.Map {
	sort.Slice(rs, func(i, j int) bool {
		return rs[i].Timestamp < rs[j].Timestamp
	})
	opt := querySchema.GetOptions()

	var filterFields []*influxql.VarRef
	var auxTags []string
	var recSchema record.Schemas
	filterFields, _ = getFilterFieldsByExpr(opt.GetCondition(), filterFields[:0])
	_, recSchema = NewRecordSchema(querySchema, auxTags[:0], recSchema[:0], filterFields)

	mm := make(map[string]*seriesData, 0)
	limit := opt.GetOffset() + opt.GetLimit()
	for i := range rs {
		if !opt.IsAscending() {
			i = len(rs) - 1 - i
		}
		point := rs[i]
		if point.Name != opt.OptionsName() || point.Timestamp < opt.GetStartTime() || point.Timestamp > opt.GetEndTime() {
			continue
		}
		var seriesKey []byte
		seriesKey = influx.Parse2SeriesKey(point.IndexKey, seriesKey)

		ptRec := transRowToRecordNew(&point, recSchema)
		newSeries := seriesData{tags: point.Tags, rec: ptRec}

		val, exist := mm[string(seriesKey)]
		if !exist {
			mm[string(seriesKey)] = &newSeries
		} else {
			if limit == 0 || (limit > 0 && val.rec.Len() < limit) {
				appendFieldValueToRecord(val.rec, point.Fields, point.Timestamp)
			}
		}
	}

	var ret sync.Map

	if len(filterFields) > 0 {
		idFields := make([]int, 0, 5)
		idTags := make([]string, 0, 4)
		var valueMap map[string]interface{}

		for _, f := range filterFields {
			idx := recSchema.FieldIndex(f.Val)
			if idx >= 0 && f.Type != influxql.Unknown {
				idFields = append(idFields, idx)
			} else if f.Type != influxql.Unknown {
				idTags = append(idTags, f.Val)
			}
		}
		valueMap = prepareForFilter(recSchema, idFields)

		// filter field value by cond, if filter rec is nil, remove from the map
		var emptyKeys []string
		for k, v := range mm {
			rec := v.rec
			filterRec := immutable.FilterByField(rec, valueMap, opt.GetCondition(), idFields, idTags, &v.tags)
			if filterRec == nil {
				emptyKeys = append(emptyKeys, k)
			} else {
				// todo: should use this method later
				// v.rec = kickFilterCol(filterRec, querySchema.GetColumnNames())
				v.rec = filterRec
			}
		}

		for i := range emptyKeys {
			delete(mm, emptyKeys[i])
		}
	}

	for k, v := range mm {
		ret.Store(k, *v.rec)
	}

	return &ret
}

func sameOffset(expectOffset, mergeOffset []uint32) bool {
	if len(expectOffset) == 0 && len(mergeOffset) == 0 {
		return true
	}
	return reflect.DeepEqual(expectOffset, mergeOffset)
}

func isRecEqual(mergeRec, expRec *record.Record) bool {
	for i := range mergeRec.Schema {
		if mergeRec.Schema[i].Name != expRec.Schema[i].Name || mergeRec.Schema[i].Type != expRec.Schema[i].Type {
			return false
		}
	}

	for i := range mergeRec.ColVals {
		if isColHaveNoData(&expRec.ColVals[i]) && isColHaveNoData(&mergeRec.ColVals[i]) {
			continue
		}
		if mergeRec.ColVals[i].Len != expRec.ColVals[i].Len ||
			mergeRec.ColVals[i].NilCount != expRec.ColVals[i].NilCount ||
			!bytes.Equal(mergeRec.ColVals[i].Val, expRec.ColVals[i].Val) ||
			!sameOffset(mergeRec.ColVals[i].Offset, expRec.ColVals[i].Offset) {
			return false
		}
		for j := 0; j < mergeRec.ColVals[i].Len; j++ {
			firstBitIndex := mergeRec.ColVals[i].BitMapOffset + j
			secBitIndex := expRec.ColVals[i].BitMapOffset + j
			firstBit, secBit := 0, 0
			if mergeRec.ColVals[i].Bitmap[firstBitIndex>>3]&record.BitMask[firstBitIndex&0x07] != 0 {
				firstBit = 1
			}
			if expRec.ColVals[i].Bitmap[secBitIndex>>3]&record.BitMask[secBitIndex&0x07] != 0 {
				secBit = 1
			}

			if firstBit != secBit {
				return false
			}
		}
	}

	return true
}

func isStringsEqual(firstStrings, secStrings []string) bool {
	if len(firstStrings) != len(secStrings) {
		return false
	}

	for i := range firstStrings {
		if !reflect.DeepEqual(firstStrings[i], secStrings[i]) {
			return false
		}
	}

	return true
}

func isColHaveNoData(cv *record.ColVal) bool {
	if cv.Val == nil && cv.NilCount == 0 && cv.Len == 0 && cv.Bitmap == nil && cv.BitMapOffset == 0 && cv.Offset == nil {
		return true
	}

	if cv.Len == cv.NilCount {
		return true
	}

	return false
}

func testRecsEqual(mergeRec, expRec *record.Record) bool {
	for i, v := range mergeRec.Schema {
		if isColHaveNoData(&expRec.ColVals[i]) && isColHaveNoData(&mergeRec.ColVals[i]) {
			continue
		}
		if v.Type == influx.Field_Type_Int {
			mergeIntegerVals := mergeRec.ColVals[i].IntegerValues()
			expIntegerVals := expRec.ColVals[i].IntegerValues()
			isEqual := reflect.DeepEqual(mergeIntegerVals, expIntegerVals)
			if !isEqual {
				return false
			}
		} else if v.Type == influx.Field_Type_Boolean {
			mergeBooleanVals := mergeRec.ColVals[i].BooleanValues()
			expBooleanVals := expRec.ColVals[i].BooleanValues()
			isEqual := reflect.DeepEqual(mergeBooleanVals, expBooleanVals)
			if !isEqual {
				return false
			}
		} else if v.Type == influx.Field_Type_Float {
			mergeFloatVals := mergeRec.ColVals[i].FloatValues()
			expFloatVals := expRec.ColVals[i].FloatValues()
			isEqual := reflect.DeepEqual(mergeFloatVals, expFloatVals)
			if !isEqual {
				return false
			}
		} else if v.Type == influx.Field_Type_String {
			mergeStrings := mergeRec.ColVals[i].StringValues(nil)
			expStrings := expRec.ColVals[i].StringValues(nil)
			isEqual := isStringsEqual(mergeStrings, expStrings)
			if !isEqual {
				return false
			}
		} else {
			panic("error type")
		}
	}

	return isRecEqual(mergeRec, expRec)
}

func closeChan(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func checkQueryResultParallel(errs chan error, cursors []comm.KeyCursor, expectRecords *sync.Map, ascending bool, function checkSingleCursorFunction) {
	var total int
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(len(cursors))
	for _, cur := range cursors {
		go func(cur comm.KeyCursor) {
			if err := function(cur, expectRecords, &total, ascending, stop); err != nil {
				wg.Done()
				errs <- err
				closeChan(stop)
			} else {
				errs <- nil
				wg.Done()
			}
		}(cur)
	}
	wg.Wait()
	closeChan(stop)
}

func checkQueryResultForSingleCursorNew(cur comm.KeyCursor, expectRecords *sync.Map, total *int, ascending bool, stop chan struct{}) error {
	defer cur.Close()
	for {
		if groupCursor, isTagSet := cur.(*groupCursor); isTagSet {
			for i := range groupCursor.tagSetCursors {
				t := groupCursor.tagSetCursors[i].(*tagSetCursor)
				t.SetSchema(t.GetSchema())
			}
		}

		select {
		case <-stop:
			return nil
		default:
		}

		rec, _, err := cur.Next()
		if err != nil {
			return err
		}
		if rec == nil {
			break
		}
		f := func() error {
			key, index := rec.GetTagIndexAndKey()
			v := rec.ColVals[len(rec.ColVals)-1].IntegerValues()
			for i := 0; i < len(key)-1; i++ {
				r := bytes.Compare(executor.DecodeBytes(*key[i]), executor.DecodeBytes(*key[i+1]))
				if (r < 0) != ascending {
					return errors.New("wrong data tag order")
				}
				for j := index[i]; j < index[i+1]-1; j++ {
					if v[j] == v[j+1] {
						continue
					}
					if (v[j] < v[j+1]) != ascending {
						return errors.New("wrong data time order")
					}
				}
			}
			for j := index[len(index)-1]; j < rec.RowNums()-1; j++ {
				if v[j] == v[j+1] {
					continue
				}
				if (v[j] < v[j+1]) != ascending {
					return errors.New("wrong data time order")
				}
			}
			return nil
		}
		if e := f(); e != nil {
			return e
		}
		*total += rec.RowNums()
	}
	return nil
}

func checkAggQueryResultParallel(errs chan error, cursors []comm.KeyCursor, expectRecords *sync.Map, ascending bool, call string, result aggResult) {
	var wg sync.WaitGroup
	var function func(cur comm.KeyCursor, stop chan struct{}, result aggResult, call string) error
	stop := make(chan struct{})
	wg.Add(len(cursors))
	function = checkAggFunc
	for _, cur := range cursors {
		go func(cur comm.KeyCursor) {
			if err := function(cur, stop, result, call); err != nil {
				wg.Done()
				errs <- err
				closeChan(stop)
			} else {
				errs <- nil
				wg.Done()
			}
		}(cur)
	}
	wg.Wait()
	closeChan(stop)
}

func checkAggFunc(cur comm.KeyCursor, stop chan struct{}, result aggResult, call string) error {
	defer cur.Close()
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		rec, _, err := cur.Next()
		if err != nil {
			return err
		}
		if rec == nil {
			return nil
		}
		if call == "count" {
			return nil
		} else {
			for i := 0; i < len(rec.Schema)-1; i++ {
				f := rec.Schema[i]
				switch f.Type {
				case influx.Field_Type_Float:
					if rec.ColVals[i].FloatValues()[0] != result[call][3].(float64) {
						return errors.New(fmt.Sprintf("unexpected value, expected: %f,actual: %f", result[call][3].(float64), rec.ColVals[i].FloatValues()[0]))
					}
				case influx.Field_Type_Int:
					if rec.ColVals[i].IntegerValues()[0] != result[call][1].(int64) {
						return errors.New(fmt.Sprintf("unexpected value, expected: %d,actual: %d", result[call][1].(int64), rec.ColVals[i].IntegerValues()[0]))
					}
				case influx.Field_Type_Boolean:
					if rec.ColVals[i].BooleanValues()[0] != result[call][2].(bool) {
						return errors.New(fmt.Sprintf("unexpected value, expected: %t,actual: %t", result[call][2].(bool), rec.ColVals[i].BooleanValues()[0]))
					}
				case influx.Field_Type_String:
					if s, _ := rec.ColVals[i].StringValueSafe(0); s != result[call][0].(string) {
						return errors.New(fmt.Sprintf("unexpected value, expected: %s,actual: %s", result[call][0].(string), s))
					}
				}
			}
		}
		if err != nil {
			return err
		}
		if rec == nil {
			break
		}
	}
	f := func() error {
		return nil
	}
	return f()
}

func checkRecord(query *record.Record, seriesKey string, expectRecords *sync.Map, name string) error {
	rowCnt := query.RowNums()
	val, ok := expectRecords.Load(seriesKey)
	if !ok {
		return fmt.Errorf("can not find record for series key %s", seriesKey)
	}
	r := val.(record.Record)
	var comp, res record.Record
	comp.SliceFromRecord(&r, 0, rowCnt)
	if !testRecsEqual(query, &comp) {
		return fmt.Errorf("%v record not equal, exp:%v\nget:%v", name, comp.String(), query.String())
	}
	// update map value
	if rowCnt < r.RowNums() {
		res.SliceFromRecord(&r, rowCnt, r.RowNums())
		expectRecords.Store(seriesKey, res)
	}
	return nil
}

func checkQueryResultForSingleCursor(cur comm.KeyCursor, expectRecords *sync.Map, total *int, ascending bool, stop chan struct{}) error {
	defer cur.Close()

	name := cur.Name()
	/*	var preKey string
		var preMergeRec *comm.Record*/
	for {
		if groupCursor, isTagSet := cur.(*groupCursor); isTagSet {
			groupCursor.preAgg = true
			for i := range groupCursor.tagSetCursors {
				groupCursor.tagSetCursors[i].(*tagSetCursor).SetNextMethod()
			}
		}

		select {
		case <-stop:
			return nil
		default:
		}

		rec, info, err := cur.Next()
		if err != nil {
			return err
		}
		if rec == nil {
			break
		}
		key := info.GetSeriesKey()
		k := string(key)
		_, ok := expectRecords.Load(k)
		if !ok {
			return fmt.Errorf("key %s not exist", k)
		}
		if err = checkRecord(rec, k, expectRecords, name); err != nil {
			return err
		}

		*total += rec.RowNums()
	}
	return nil
}

type TestConfig struct {
	seriesNum         int
	pointNumPerSeries int
	interval          time.Duration
	short             bool
}

type TestCase struct {
	Name               string            // testcase name
	startTime, endTime int64             // query time range
	fieldAux           []influxql.VarRef // fields to select
	fieldFilter        string            // field filter condition
	dims               []string          // group by dimensions
	ascending          bool              // order by time
}

type LimitTestParas struct {
	limit  int
	offset int
}

func TestAggQueryOnlyInImmutable(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{20, 1, time.Second, false},
		{30, 2, time.Second, false},
		{50, 3, time.Second, false},
	}
	for _, config := range configs {
		executor.EnableFileCursor(true)
		if testing.Short() && config.short {
			t.Skip("skipping test in short mode.")
		}
		// step1: clean env
		_ = os.RemoveAll(testDir)

		// step2: create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
		rows, minTime, maxTime, result := GenAggDataRecord([]string{"cpu"}, config.seriesNum, config.pointNumPerSeries, config.interval, time.Now(), false, true, false)
		err = writeData(sh, rows, true)
		if err != nil {
			t.Fatal(err)
		}
		err = writeData(sh, rows, true)
		if err != nil {
			t.Fatal(err)
		}
		// query data and judge
		cases := []TestAggCase{
			{"PartFieldFilter_all_columns", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int", "field3_bool", "field4_float"}), "", nil, true, []string{"first", "last", "count"}},
			{"PartFieldFilter_all_columns_except_string", minTime, maxTime, createFieldAux([]string{"field2_int", "field3_bool", "field4_float"}), "", nil, true, []string{"min", "max", "first", "last", "count"}},
			{"PartFieldFilter_all_columns_int_float", minTime, maxTime, createFieldAux([]string{"field2_int", "field4_float"}), "", nil, true, []string{"first", "min", "max", "last", "count", "sum"}},
			{"PartFieldFilter_single_column_string", minTime, maxTime, createFieldAux([]string{"field1_string"}), "", nil, true, []string{"first", "last", "count"}},
			{"PartFieldFilter_single_column_int", minTime, maxTime, createFieldAux([]string{"field2_int"}), "", nil, true, []string{"first", "min", "max", "last", "count", "sum"}},
			{"PartFieldFilter_single_column_bool", minTime, maxTime, createFieldAux([]string{"field3_bool"}), "", nil, true, []string{"first", "last", "min", "max", "count"}},
			{"PartFieldFilter_single_column_float", minTime, maxTime, createFieldAux([]string{"field4_float"}), "", nil, true, []string{"first", "min", "max", "last", "count", "sum"}},
		}
		chunkSize := []int{1, 2}
		timeOrder := []bool{true, false}
		for _, ascending := range timeOrder {
			for _, c := range cases {
				for _, size := range chunkSize {
					c := c
					ascending := ascending
					t.Run(c.Name, func(t *testing.T) {
						for i := range c.aggCall {
							opt := genAggQueryOpt(&c, "cpu", ascending, size, config.interval)
							calls := genCall(c.fieldAux, c.aggCall[i])
							querySchema := genAggQuerySchema(c.fieldAux, calls, opt)
							ops := genOps(c.fieldAux, calls)
							cursors, err := sh.CreateCursor(context.Background(), querySchema)
							if err != nil {
								t.Fatal(err)
							}

							updateClusterCursor(cursors, ops, c.aggCall[i])
							// step5: loop all cursors to query data from shard
							// key is indexKey, value is Record
							m := genExpectRecordsMap(rows, querySchema)
							errs := make(chan error, len(cursors))
							checkAggQueryResultParallel(errs, cursors, m, ascending, c.aggCall[i], *result)
							close(errs)
							for i := 0; i < len(cursors); i++ {
								err = <-errs
								if err != nil {
									t.Fatal(err)
								}
							}
						}
					})
				}
			}

		}
		// step6: close shard
		err = closeShard(sh)
		if err != nil {
			t.Fatal(err)
		}
	}
	executor.EnableFileCursor(false)
}

// data: all data in immutable
func TestQueryOnlyInImmutable(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
		{100000, 2, time.Second, true},
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	checkFunctions := []checkSingleCursorFunction{checkQueryResultForSingleCursor, checkQueryResultForSingleCursorNew}
	for _, f := range checkFunctions {
		for index := range configs {
			if testing.Short() && configs[index].short {
				t.Skip("skipping test in short mode.")
			}
			// step1: clean env
			_ = os.RemoveAll(testDir)

			// step2: create shard
			sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
			if err != nil {
				t.Fatal(err)
			}

			// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
			rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
			err = writeData(sh, rows, true)
			if err != nil {
				t.Fatal(err)
			}
			for nameIdx := range msNames {
				// query data and judge
				cases := []TestCase{
					{"AllFieldFilter", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
					{"PartFieldFilter", minTime, maxTime, createFieldAux([]string{"field1_string", "field3_bool"}), "field2_int < 5 AND field4_float < 10.0", nil, true},
					{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
					{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, true},
					{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
					{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
					{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
					{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"OverlapTime2", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
					{"TagNotEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
				}

				timeOrder := []bool{true, false}
				for _, ascending := range timeOrder {
					for _, c := range cases {
						c := c
						ascending := ascending
						t.Run(c.Name, func(t *testing.T) {

							opt := genQueryOpt(&c, msNames[nameIdx], ascending)
							querySchema := genQuerySchema(c.fieldAux, opt)
							cursors, err := sh.CreateCursor(context.Background(), querySchema)
							if err != nil {
								t.Fatal(err)
							}

							// step5: loop all cursors to query data from shard
							// key is indexKey, value is Record
							m := genExpectRecordsMap(rows, querySchema)
							errs := make(chan error, len(cursors))
							checkQueryResultParallel(errs, cursors, m, ascending, f)
							close(errs)
							for i := 0; i < len(cursors); i++ {
								err = <-errs
								if err != nil {
									t.Fatal(err)
								}
							}
						})

					}
				}
			}
			// step6: close shard
			err = closeShard(sh)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// data: all data in immutable
func TestQueryOnlyInImmutableWithLimit(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 1001, time.Second, false},
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	checkFunctions := []checkSingleCursorFunction{checkQueryResultForSingleCursor, checkQueryResultForSingleCursorNew}
	for _, f := range checkFunctions {
		for index := range configs {
			if testing.Short() && configs[index].short {
				t.Skip("skipping test in short mode.")
			}
			// step1: clean env
			_ = os.RemoveAll(testDir)

			// step2: create shard
			sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
			if err != nil {
				t.Fatal(err)
			}

			// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
			rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
			err = writeData(sh, rows, true)
			if err != nil {
				t.Fatal(err)
			}
			for nameIdx := range msNames {
				// query data and judge
				cases := []TestCase{
					{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
					{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
					{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, true},
					{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
					{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
					{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
					{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"OverlapTime", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
					{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
					{"TagNotEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
				}

				timeOrder := []bool{true, false}
				for _, ascending := range timeOrder {
					for _, c := range cases {
						c := c
						LimitTestCases := []LimitTestParas{
							{offset: 100, limit: 100},
						}
						for _, limitCase := range LimitTestCases {
							t.Run(c.Name, func(t *testing.T) {
								opt := genQueryOpt(&c, msNames[nameIdx], ascending)
								opt.Limit = limitCase.limit
								opt.Offset = limitCase.offset
								querySchema := genQuerySchema(c.fieldAux, opt)

								cursors, err := sh.CreateCursor(context.Background(), querySchema)
								if err != nil {
									t.Fatal(err)
								}

								// step5: loop all cursors to query data from shard
								// key is indexKey, value is Record
								m := genExpectRecordsMap(rows, querySchema)
								errs := make(chan error, len(cursors))
								checkQueryResultParallel(errs, cursors, m, ascending, f)
								close(errs)
								for i := 0; i < len(cursors); i++ {
									err = <-errs
									if err != nil {
										t.Fatal(err)
									}
								}
							})
						}
					}
				}
			}
			// step6: close shard
			err = closeShard(sh)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestQueryOnlyInImmutableWithLimitOptimize(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 1001, time.Second, false},
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	checkFunctions := []checkSingleCursorFunction{checkQueryResultForSingleCursor, checkQueryResultForSingleCursorNew}
	for _, f := range checkFunctions {
		for index := range configs {
			if testing.Short() && configs[index].short {
				t.Skip("skipping test in short mode.")
			}
			// step1: clean env
			_ = os.RemoveAll(testDir)

			// step2: create shard
			sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
			if err != nil {
				t.Fatal(err)
			}

			nowTime := time.Now()

			// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
			rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, nowTime, false, true, true)
			err = writeData(sh, rows, true)
			if err != nil {
				t.Fatal(err)
			}

			rows2, _, _ := GenDataRecord(msNames, configs[index].seriesNum-100, configs[index].pointNumPerSeries, configs[index].interval, nowTime, false, true, true)
			err = writeData(sh, rows2, true)
			if err != nil {
				t.Fatal(err)
			}
			for nameIdx := range msNames {
				// query data and judge
				cases := []TestCase{
					{"AllField", minTime, maxTime, createFieldAux(nil), "", nil, true},
				}

				timeOrder := []bool{true, false}
				for _, ascending := range timeOrder {
					for _, c := range cases {
						c := c
						LimitTestCases := []LimitTestParas{
							{offset: 1, limit: 5},
						}
						for _, limitCase := range LimitTestCases {
							t.Run(c.Name, func(t *testing.T) {
								opt := genQueryOpt(&c, msNames[nameIdx], ascending)
								opt.Limit = limitCase.limit
								opt.Offset = limitCase.offset
								querySchema := genQuerySchema(c.fieldAux, opt)

								cursors, err := sh.CreateCursor(context.Background(), querySchema)
								if err != nil {
									t.Fatal(err)
								}

								// step5: loop all cursors to query data from shard
								// key is indexKey, value is Record
								m := genExpectRecordsMap(rows, querySchema)
								errs := make(chan error, len(cursors))
								checkQueryResultParallel(errs, cursors, m, ascending, f)
								close(errs)
								for i := 0; i < len(cursors); i++ {
									err = <-errs
									if err != nil {
										t.Fatal(err)
									}
								}
							})
						}
					}
				}
			}
			// step6: close shard
			err = closeShard(sh)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestQueryOnlyInImmutableWithLimitWithGroupBy(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
	}
	checkFunctions := []checkSingleCursorFunction{checkQueryResultForSingleCursor, checkQueryResultForSingleCursorNew}
	for _, f := range checkFunctions {
		msNames := []string{"cpu", "cpu1", "disk"}
		for index := range configs {
			if testing.Short() && configs[index].short {
				t.Skip("skipping test in short mode.")
			}
			// step1: clean env
			os.RemoveAll(testDir)

			// step2: create shard
			sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
			if err != nil {
				t.Fatal(err)
			}

			// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
			rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
			err = writeData(sh, rows, true)
			if err != nil {
				t.Fatal(err)
			}
			for nameIdx := range msNames {
				// query data and judge
				cases := []TestCase{
					{"AllDims", minTime, maxTime, createFieldAux(nil), "", []string{"tagkey1", "tagkey2", "tagkey3", "tagkey4"}, false},
					{"PartDims1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", []string{"tagkey1", "tagkey2"}, false},
					{"PartDims2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", []string{"tagkey3", "tagkey4"}, false},
					{"PartDims3", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", []string{"tagkey1"}, false},
				}

				for _, c := range cases {
					c := c
					LimitTestCases := []LimitTestParas{
						{offset: 100, limit: 100},
					}
					ascending := true
					for _, limitCase := range LimitTestCases {
						t.Run(c.Name, func(t *testing.T) {
							opt := genQueryOpt(&c, msNames[nameIdx], ascending)
							opt.Limit = limitCase.limit
							opt.Offset = limitCase.offset
							querySchema := genQuerySchema(c.fieldAux, opt)

							cursors, err := sh.CreateCursor(context.Background(), querySchema)
							if err != nil {
								t.Fatal(err)
							}

							// step5: loop all cursors to query data from shard
							// key is indexKey, value is Record
							m := genExpectRecordsMap(rows, querySchema)
							errs := make(chan error, len(cursors))
							checkQueryResultParallel(errs, cursors, m, ascending, f)
							close(errs)
							for i := 0; i < len(cursors); i++ {
								err = <-errs
								if err != nil {
									t.Fatal(err)
								}
							}
						})
					}
				}
			}
			// step6: close shard
			err = closeShard(sh)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// data: all data in immutable
func TestQueryOnlyInImmutableGroupBy(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
	}
	checkFunctions := []checkSingleCursorFunction{checkQueryResultForSingleCursor, checkQueryResultForSingleCursorNew}
	msNames := []string{"cpu", "cpu1", "disk"}
	for _, f := range checkFunctions {
		for index := range configs {
			if testing.Short() && configs[index].short {
				t.Skip("skipping test in short mode.")
			}
			// step1: clean env
			_ = os.RemoveAll(testDir)

			// step2: create shard
			sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
			if err != nil {
				t.Fatal(err)
			}

			// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
			rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
			err = writeData(sh, rows, true)
			if err != nil {
				t.Fatal(err)
			}
			for nameIdx := range msNames {
				// query data and judge
				cases := []TestCase{
					{"AllDims", minTime, maxTime, createFieldAux(nil), "", []string{"tagkey1", "tagkey2", "tagkey3", "tagkey4"}, false},
					{"PartDims1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", []string{"tagkey1", "tagkey2"}, false},
					{"PartDims2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", []string{"tagkey3", "tagkey4"}, false},
					{"PartDims3", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", []string{"tagkey1"}, false},
				}

				timeOrder := []bool{true, false}

				for _, ascending := range timeOrder {
					for _, c := range cases {
						c := c
						ascending := ascending
						t.Run(c.Name, func(t *testing.T) {
							opt := genQueryOpt(&c, msNames[nameIdx], ascending)
							querySchema := genQuerySchema(c.fieldAux, opt)
							cursors, err := sh.CreateCursor(context.Background(), querySchema)
							if err != nil {
								t.Fatal(err)
							}

							// step5: loop all cursors to query data from shard
							// key is indexKey, value is Record
							m := genExpectRecordsMap(rows, querySchema)
							errs := make(chan error, len(cursors))
							checkQueryResultParallel(errs, cursors, m, ascending, f)
							close(errs)
							for i := 0; i < len(cursors); i++ {
								err = <-errs
								if err != nil {
									t.Fatal(err)
								}
							}
						})
					}
				}
			}
			// step6: close shard
			err = closeShard(sh)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

//
// data: all data in mutable
func TestQueryOnlyInMutableTable(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{1001, 1, time.Second, false},
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
		{100000, 2, time.Second, true},
	}
	msNames := []string{"cpu", "mem", "disk"}
	for index := range configs {
		if testing.Short() && configs[index].short {
			t.Skip("skipping test in short mode.")
		}
		// step1: clean env
		_ = os.RemoveAll(testDir)

		// step2: create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// not flush data to snapshot
		sh.SetWriteColdDuration(1000 * time.Second)
		sh.SetMutableSizeLimit(10000000000)

		// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
		rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
		err = writeData(sh, rows, false)
		if err != nil {
			t.Fatal(err)
		}
		for nameIdx := range msNames {
			// query data and judge
			cases := []TestCase{
				{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
				{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
				{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, true},
				{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
				{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"OverlapTime", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
				{"TagNotEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
			}

			timeOrder := []bool{true, false}

			for _, ascending := range timeOrder {
				for _, c := range cases {
					c := c
					ascending := ascending
					t.Run(c.Name, func(t *testing.T) {

						opt := genQueryOpt(&c, msNames[nameIdx], ascending)
						querySchema := genQuerySchema(c.fieldAux, opt)
						cursors, err := sh.CreateCursor(context.Background(), querySchema)
						if err != nil {
							t.Fatal(err)
						}

						// step5: loop all cursors to query data from shard
						// key is indexKey, value is Record
						m := genExpectRecordsMap(rows, querySchema)

						errs := make(chan error, len(cursors))
						checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)
						close(errs)
						for i := 0; i < len(cursors); i++ {
							err = <-errs
							if err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			}

		}

		// step6: close shard
		err = closeShard(sh)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// data: all data in immutable, write twice, time range are equal
// select tag/field: no tag, all field
// condition: no tag filter, query different time range
func TestQueryImmutableUnorderedNoOverlap(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
		{100000, 2, time.Second, true},
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	for index := range configs {
		if testing.Short() && configs[index].short {
			t.Skip("skipping test in short mode.")
		}
		_ = os.RemoveAll(testDir)
		// create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// first write
		tm := time.Now()
		rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm, false, true, false)
		err = writeData(sh, rows, true)
		if err != nil {
			t.Fatal(err)
		}

		// second write, same data(shardkey and timestamp)
		tm2 := tm
		rows2, minTime2, maxTime2 := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm2, false, true, false)
		err = writeData(sh, rows2, true)
		if err != nil {
			t.Fatal(err)
		}

		if minTime != minTime2 || maxTime != maxTime2 {
			t.Fatalf("time range error, %d, %d", maxTime2, minTime)
		}
		for nameIdx := range msNames {

			// query data and judge
			cases := []TestCase{
				{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
				{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
				{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, true},
				{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
				{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"OverlapTime1", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
			}

			timeOrder := []bool{true, false}

			for _, ascending := range timeOrder {
				for _, c := range cases {
					c := c
					t.Run(c.Name, func(t *testing.T) {

						opt := genQueryOpt(&c, msNames[nameIdx], ascending)
						querySchema := genQuerySchema(c.fieldAux, opt)
						cursors, err := sh.CreateCursor(context.Background(), querySchema)
						if err != nil {
							t.Fatal(err)
						}

						// step5: loop all cursors to query data from shard
						// key is indexKey, value is Record
						m := genExpectRecordsMap(rows2, querySchema)

						errs := make(chan error, len(cursors))
						checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)
						close(errs)
						for i := 0; i < len(cursors); i++ {
							err = <-errs
							if err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			}
		}

		// step6: close shard
		err = closeShard(sh)
		if err != nil {
			t.Fatal(err)
		}
	}
}

//
// data: all data in mutable, write twice, time range are equal
// select tag/field: no tag, all field
// condition: no tag filter, query different time range
func TestQueryMutableUnorderedNoOverlap(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
		{200, 1001, time.Second, true},
		{100, 2321, time.Second, true},
		{100000, 2, time.Second, true},
	}
	msNames := []string{"cpu", "mem", "disk"}
	for index := range configs {
		if testing.Short() && configs[index].short {
			t.Skip("skipping test in short mode.")
		}
		_ = os.RemoveAll(testDir)
		// create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// first write
		tm := time.Now()
		rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm, false, true, false)
		err = writeData(sh, rows, false)
		if err != nil {
			t.Fatal(err)
		}

		// second write, same data(shardkey and timestamp)
		tm2 := tm
		rows2, minTime2, maxTime2 := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm2, false, true, false)
		err = writeData(sh, rows2, false)
		if err != nil {
			t.Fatal(err)
		}

		if minTime != minTime2 || maxTime != maxTime2 {
			t.Fatalf("time range error, %d, %d", maxTime2, minTime)
		}
		for nameIdx := range msNames {
			// query data and judge
			cases := []TestCase{
				{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
				{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
				{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, true},
				{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
				{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"OverlapTime", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, true},
			}

			timeOrder := []bool{true, false}

			for _, ascending := range timeOrder {
				for _, c := range cases {
					c := c
					t.Run(c.Name, func(t *testing.T) {
						opt := genQueryOpt(&c, msNames[nameIdx], ascending)
						querySchema := genQuerySchema(c.fieldAux, opt)
						cursors, err := sh.CreateCursor(context.Background(), querySchema)
						if err != nil {
							t.Fatal(err)
						}

						// step5: loop all cursors to query data from shard
						// key is indexKey, value is Record
						m := genExpectRecordsMap(rows2, querySchema)

						errs := make(chan error, len(cursors))
						checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)
						close(errs)
						for i := 0; i < len(cursors); i++ {
							err = <-errs
							if err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			}
		}

		// step6: close shard
		err = closeShard(sh)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// data: all data in mutable, write twice, time range are equal
// select tag/field: no tag, all field
// condition: no tag filter, query different time range
func TestQueryImmutableSequenceWrite(t *testing.T) {
	testDir := t.TempDir()
	configs := []TestConfig{
		{1001, 1, time.Second, false},
		{20, 2, time.Second, false},
		{2, 1001, time.Second, false},
		{100, 2321, time.Second, true},
	}
	msNames := []string{"cpu", "mem", "disk"}
	for index := range configs {
		if testing.Short() && configs[index].short {
			t.Skip("skipping test in short mode.")
		}
		_ = os.RemoveAll(testDir)
		// create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// first write
		tm := time.Now().Truncate(time.Second)
		rows, minTime, maxTime := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm, false, true, false)
		err = writeData(sh, rows, true)
		if err != nil {
			t.Fatal(err)
		}

		// second write, start time larger than last write end time
		tm2 := time.Unix(0, maxTime+int64(time.Second))
		rows2, minTime2, maxTime2 := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, tm2, false, true, false)
		err = writeData(sh, rows2, true)
		if err != nil {
			t.Fatal(err)
		}

		totalRow := rows
		totalRow = append(totalRow, rows2...)
		if maxTime >= minTime2 || minTime2 > maxTime2 {
			t.Fatalf("time range error, %d, %d", maxTime2, minTime)
		}
		for nameIdx := range msNames {
			// query data and judge
			cases := []TestCase{
				{"AllField", minTime, maxTime2, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, true},
				{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, true},
				{"BeyondMaxTime", maxTime2 + int64(time.Second), maxTime2 + int64(time.Second), createFieldAux(nil), "", nil, true},
				{"OverlapTime", minTime + 20*int64(time.Second), maxTime2 - 10*int64(time.Second), createFieldAux(nil), "", nil, true},
				{"partField1", minTime, maxTime2, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"partField2", minTime, maxTime2, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, true},
				{"BeyondMaxTime + partField2", maxTime2 + int64(time.Second), maxTime2 + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"OverlapTime2", minTime + 10*int64(time.Second), maxTime2 - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, true},
				{"TagEqual", minTime, maxTime2, createFieldAux(nil), "", nil, true},
			}

			for _, c := range cases {
				c := c
				ascending := true
				t.Run(c.Name, func(t *testing.T) {
					opt := genQueryOpt(&c, msNames[nameIdx], ascending)
					querySchema := genQuerySchema(c.fieldAux, opt)
					cursors, err := sh.CreateCursor(context.Background(), querySchema)
					if err != nil {
						t.Fatal(err)
					}

					// step5: loop all cursors to query data from shard
					// key is indexKey, value is Record
					m := genExpectRecordsMap(totalRow, querySchema)
					errs := make(chan error, len(cursors))
					checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)
					close(errs)
					for i := 0; i < len(cursors); i++ {
						err = <-errs
						if err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}

		// step6: close shard
		err = closeShard(sh)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueryOnlyInImmutableReload(t *testing.T) {
	testDir := t.TempDir()
	config := TestConfig{100, 1001, time.Second, false}
	if testing.Short() && config.short {
		t.Skip("skipping test in short mode.")
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	// step1: clean env
	_ = os.RemoveAll(testDir)

	// step2: create shard
	sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
	if err != nil {
		t.Fatal(err)
	}

	// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
	rows, minTime, maxTime := GenDataRecord(msNames, config.seriesNum, config.pointNumPerSeries, config.interval, time.Now(), false, true, false)
	err = writeData(sh, rows, true)
	if err != nil {
		t.Fatal(err)
	}

	if err = sh.immTables.Close(); err != nil {
		t.Fatal(err)
	}

	sh.immTables = immutable.NewTableStore(sh.tsspPath, &sh.tier, true, immutable.NewConfig())
	if _, _, err = sh.immTables.Open(); err != nil {
		t.Fatal(err)
	}

	for nameIdx := range msNames {
		// query data and judge
		cases := []TestCase{
			{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, false},
			{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, false},
			{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, false},
			{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, false},
			{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, false},
			{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, false},
			{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"OverlapTime", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, false},
		}

		ascending := true
		for _, c := range cases {
			c := c
			t.Run(c.Name, func(t *testing.T) {
				opt := genQueryOpt(&c, msNames[nameIdx], ascending)
				querySchema := genQuerySchema(c.fieldAux, opt)
				cursors, err := sh.CreateCursor(context.Background(), querySchema)
				if err != nil {
					t.Fatal(err)
				}

				// step5: loop all cursors to query data from shard
				// key is indexKey, value is Record
				m := genExpectRecordsMap(rows, querySchema)
				errs := make(chan error, len(cursors))
				checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)

				close(errs)
				for i := 0; i < len(cursors); i++ {
					err = <-errs
					if err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
	// step6: close shard
	err = closeShard(sh)
	if err != nil {
		t.Fatal(err)
	}
}

func addFilterFieldCondition(filterCondition string, opt *query.ProcessorOptions) {
	if filterCondition == "" {
		return
	}
	fieldMap := make(map[string]influxql.DataType, 4)
	fieldMap["field1_string"] = influxql.String
	fieldMap["field2_int"] = influxql.Integer
	fieldMap["field3_bool"] = influxql.Boolean
	fieldMap["field4_float"] = influxql.Float

	opt.Condition = influxql.MustParseExpr(filterCondition)
	influxql.WalkFunc(opt.Condition, func(n influxql.Node) {
		ref, ok := n.(*influxql.VarRef)
		if !ok {
			return
		}
		ty, ok := fieldMap[ref.Val]
		if ok {
			ref.Type = ty
		}
	})
}

// generate new record by query schema, need rid cols which only used for filter
// eg, select f1, f2 from mst where f3 > 1, we should kick column 'f3'
func kickFilterCol(rec *record.Record, fields []string) *record.Record {
	var dstSchema record.Schemas
	ridIdx := make(map[int]struct{}, 0)

	for i := range rec.Schema {
		srcSchema := rec.Schema[i]
		for j := range fields {
			if srcSchema.Name == fields[j] || srcSchema.Name == record.TimeField {
				dstSchema = append(dstSchema, record.Field{Name: srcSchema.Name, Type: srcSchema.Type})
				ridIdx[i] = struct{}{}
				break
			}
		}
	}

	dstRec := record.NewRecordBuilder(dstSchema)
	rowNum := rec.RowNums()
	srcSchema := rec.Schema
	var dstIdx int
	for i := range srcSchema {
		if _, exist := ridIdx[i]; exist {
			dstRec.ColVals[dstIdx].AppendColVal(&rec.ColVals[i], srcSchema[i].Type, 0, rowNum)
			dstIdx++
		}
	}
	return dstRec
}

func prepareForFilter(schema record.Schemas, idFields []int) map[string]interface{} {
	initMap := map[int]interface{}{
		influx.Field_Type_Float:   (*float64)(nil),
		influx.Field_Type_Int:     (*int64)(nil),
		influx.Field_Type_String:  (*string)(nil),
		influx.Field_Type_Boolean: (*bool)(nil),
	}
	// init map
	valueMap := make(map[string]interface{})
	for _, id := range idFields {
		valueMap[schema[id].Name] = initMap[schema[id].Type]
	}
	return valueMap
}

func TestCheckRecordLen(t *testing.T) {
	tagsetCursor := &tagSetCursor{
		limitCount:      0,
		outOfLimitBound: false,
		schema: genQuerySchema(nil, &query.ProcessorOptions{
			Limit:       10,
			Offset:      10,
			ChunkedSize: 1000,
		}),
		heapCursor: &heapCursor{items: make([]*TagSetCursorItem, 10)},
	}
	if !tagsetCursor.CheckRecordLen() {
		t.Fatal("result wrong")
	}
}

func TestDropMeasurement(t *testing.T) {
	testDir := t.TempDir()
	msNames := []string{"cpu", "cpu1"}

	// step2: create shard
	sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
	if err != nil {
		t.Fatal(err)
	}

	tm := time.Now().Truncate(time.Second)
	// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
	rows, _, _ := GenDataRecord(msNames, 10, 20, time.Second, tm, false, true, false)
	err = writeData(sh, rows, true)
	if err != nil {
		t.Fatal(err)
	}

	tm = tm.Add(time.Second * 10)
	rows, _, _ = GenDataRecord(msNames, 10, 20, time.Second, tm, false, true, false)
	err = writeData(sh, rows, true)
	if err != nil {
		t.Fatal(err)
	}

	store := sh.TableStore()

	if store.GetOutOfOrderFileNum() != 2 {
		t.Fatal("store.GetOutOfOrderFileNum() != 2")
	}
	orderFiles := store.GetFilesRef(msNames[0], true)
	unorderedFiles := store.GetFilesRef(msNames[0], false)
	if len(unorderedFiles) != 1 {
		t.Fatalf("len(unorderedFiles) != 1")
	}
	if len(orderFiles) != 2 {
		t.Fatalf("len(orderFiles) != 2")
	}

	immutable.UnrefFiles(orderFiles...)
	immutable.UnrefFiles(unorderedFiles...)
	if err := sh.DropMeasurement(context.TODO(), msNames[0]); err != nil {
		t.Fatal(err)
	}

	orderFiles = store.GetFilesRef(msNames[1], true)
	unorderedFiles = store.GetFilesRef(msNames[1], false)
	if len(unorderedFiles) != 1 {
		t.Fatalf("len(unorderedFiles) != 1")
	}
	if len(orderFiles) != 2 {
		t.Fatalf("len(orderFiles) != 2")
	}
	immutable.UnrefFiles(orderFiles...)
	immutable.UnrefFiles(unorderedFiles...)
	if err := sh.DropMeasurement(context.TODO(), msNames[1]); err != nil {
		t.Fatal(err)
	}

	if store.GetOutOfOrderFileNum() != 0 {
		t.Fatal("store.GetOutOfOrderFileNum() != 0")
	}

	// step4: close shard
	err = closeShard(sh)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEngine_DropMeasurement(t *testing.T) {
	dir := t.TempDir()
	eng, err := initEngine1(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = eng.Close()
	}()

	msNames := []string{"cpu", "cpu1"}
	tm := time.Now().Truncate(time.Second)
	// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
	rows, _, _ := GenDataRecord(msNames, 10, 200, time.Second, tm, false, true, false)
	for len(rows) > 0 {
		if len(rows) > 200 {
			if err := eng.WriteRows("db0", "rp0", 0, 1, rows[:200], nil); err != nil {
				t.Fatal(err)
			}
			rows = rows[100:]
			continue
		}

		if err := eng.WriteRows("db0", "rp0", 0, 1, rows, nil); err != nil {
			t.Fatal(err)
		}
		rows = rows[len(rows):]
	}

	dbInfo := eng.DBPartitions["db0"][0]
	idx := dbInfo.indexBuilder[659].GetPrimaryIndex().(*tsi.MergeSetIndex)
	idx.DebugFlush()

	ret, err := eng.SeriesExactCardinality("db0", []uint32{0}, [][]byte{[]byte(msNames[0]), []byte(msNames[1])}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ret) != len(msNames) {
		t.Fatalf("len(ret) != %v", len(msNames))
	}
	for _, mst := range msNames {
		n, ok := ret[mst]
		if !ok || n < 1 {
			t.Fatalf("series cardinality < %v", 1)
		}
	}

	if err = eng.DropMeasurement("db0", "rp0", msNames[0], []uint64{1}); err != nil {
		t.Fatal(err)
	}

	ret, err = eng.SeriesExactCardinality("db0", []uint32{0}, [][]byte{[]byte(msNames[0]), []byte(msNames[1])}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ret) != 2 {
		t.Fatalf("len(ret) != %v", 1)
	}

	n, _ := ret[msNames[0]]
	if n != 0 {
		t.Fatalf("index dropped fail, %v exist after drop", msNames[0])
	}

	n, ok := ret[msNames[1]]
	if !ok || n < 1 {
		t.Fatalf("ret[i].Name < %v", msNames[1])
	}
}

func TestEngine_Statistics_Shard(t *testing.T) {
	t.Skip("skip for occasional failure")
	testDir := t.TempDir()
	configs := []TestConfig{
		{200, 2, time.Second, false},
	}
	msNames := []string{"cpu", "cpu1", "disk"}
	for index := range configs {
		if testing.Short() && configs[index].short {
			t.Skip("skipping test in short mode.")
		}
		// step1: clean env
		os.RemoveAll(testDir)

		// step2: create shard
		sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
		if err != nil {
			t.Fatal(err)
		}

		// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
		rows, _, _ := GenDataRecord(msNames, configs[index].seriesNum, configs[index].pointNumPerSeries, configs[index].interval, time.Now(), false, true, false)
		err = writeData(sh, rows, true)
		if err != nil {
			t.Fatal(err)
		}

		var bufferPool = bufferpool.NewByteBufferPool(0)
		buf := bufferPool.Get()
		buf, err = sh.Statistics(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(buf), "cpu") {
			t.Fatalf("no cpu stats in buf")
		}

		if !strings.Contains(string(buf), "cpu1") {
			t.Fatalf("no cpu1 stats in buf")
		}

		if !strings.Contains(string(buf), "disk") {
			t.Fatalf("no disk stats in buf")
		}
	}
}

func TestSnapshotLimitTsspFiles(t *testing.T) {
	testDir := t.TempDir()
	msNames := []string{"cpu"}
	// step1: clean env
	_ = os.RemoveAll(testDir)

	// step2: create shard
	sh, err := createShard(defaultDb, defaultRp, defaultPtId, testDir)
	if err != nil {
		t.Fatal(err)
	}

	var rows []influx.Row
	var minTime, maxTime int64
	st := time.Now().Truncate(time.Second)

	immutable.SetMaxRowsPerSegment(16)
	immutable.SetMaxSegmentLimit(5)
	lines := 16*5 - 1
	// step3: write data, mem table row limit less than row cnt, query will get record from both mem table and immutable
	rows, minTime, maxTime = GenDataRecord([]string{"mst"}, 4, lines, time.Second, st, true, true, false, 1)
	st = time.Unix(0, maxTime+time.Second.Nanoseconds())
	rows1, min, max := GenDataRecord([]string{"mst"}, 1, lines+1+10, time.Second, st, true, true, false, 1+4)
	rows = append(rows, rows1...)
	if min < minTime {
		minTime = min
	}
	if max > maxTime {
		maxTime = max
	}

	err = writeData(sh, rows, true)
	if err != nil {
		t.Fatal(err)
	}

	files := sh.immTables.GetFilesRef("mst", true)
	if len(files) != 2 {
		t.Fatalf("wire fail, exp:2 files, get:%v files", len(files))
	}
	immutable.UnrefFiles(files...)

	st = time.Unix(0, minTime)
	rows1, _, _ = GenDataRecord([]string{"mst"}, 1, lines+1+10, time.Second, st, true, true, false, 1+4)
	err = writeData(sh, rows1, true)
	if err != nil {
		t.Fatal(err)
	}

	files = sh.immTables.GetFilesRef("mst", false)
	if len(files) != 2 {
		t.Fatalf("wire fail, exp:2 files, get:%v files", len(files))
	}
	immutable.UnrefFiles(files...)

	for nameIdx := range msNames {
		// query data and judge
		cases := []TestCase{
			{"AllField", minTime, maxTime, createFieldAux(nil), "field2_int < 5 AND field4_float < 10.0", nil, false},
			{"BelowMinTime", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux(nil), "", nil, false},
			{"BeyondMaxTime", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux(nil), "", nil, false},
			{"OverlapTime", minTime + 20*int64(time.Second), maxTime - 10*int64(time.Second), createFieldAux(nil), "", nil, false},
			{"partField1", minTime, maxTime, createFieldAux([]string{"field1_string", "field2_int"}), "", nil, false},
			{"partField2", minTime, maxTime, createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"BelowMinTime + partField1", minTime - int64(time.Second), minTime - int64(time.Second), createFieldAux([]string{"field1_string", "field2_int"}), "", nil, false},
			{"BeyondMaxTime + partField2", maxTime + int64(time.Second), maxTime + int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"OverlapTime", minTime + 10*int64(time.Second), maxTime - 30*int64(time.Second), createFieldAux([]string{"field3_bool", "field4_float"}), "", nil, false},
			{"TagEqual", minTime, maxTime, createFieldAux(nil), "", nil, false},
		}

		ascending := true
		for _, c := range cases {
			c := c
			t.Run(c.Name, func(t *testing.T) {
				opt := genQueryOpt(&c, msNames[nameIdx], ascending)
				querySchema := genQuerySchema(c.fieldAux, opt)
				cursors, err := sh.CreateCursor(context.Background(), querySchema)
				if err != nil {
					t.Fatal(err)
				}

				// step5: loop all cursors to query data from shard
				// key is indexKey, value is Record
				m := genExpectRecordsMap(rows, querySchema)
				errs := make(chan error, len(cursors))
				checkQueryResultParallel(errs, cursors, m, ascending, checkQueryResultForSingleCursor)

				close(errs)
				for i := 0; i < len(cursors); i++ {
					err = <-errs
					if err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
	// step6: close shard
	err = closeShard(sh)
	if err != nil {
		t.Fatal(err)
	}
}

type TestAggCase struct {
	Name               string            // testcase name
	startTime, endTime int64             // query time range
	fieldAux           []influxql.VarRef // fields to select
	fieldFilter        string            // field filter condition
	dims               []string          // group by dimensions
	ascending          bool              // order by time
	aggCall            []string
}

func genAggQuerySchema(fieldAux []influxql.VarRef, callField []influxql.Call, opt *query.ProcessorOptions) *executor.QuerySchema {
	var fields influxql.Fields
	var columnNames []string
	for i := range callField {
		f := &influxql.Field{
			Expr:  &callField[i],
			Alias: callField[i].Name,
		}
		fields = append(fields, f)
		columnNames = append(columnNames, callField[i].Name)
	}
	for i := range fieldAux {
		f := &influxql.Field{
			Expr:  &fieldAux[i],
			Alias: "",
		}
		fields = append(fields, f)
		columnNames = append(columnNames, fieldAux[i].Val)
	}
	return executor.NewQuerySchema(fields, columnNames, opt)
}

func genAggQueryOpt(tc *TestAggCase, msName string, ascending bool, chunkSize int, interval time.Duration) *query.ProcessorOptions {
	var opt query.ProcessorOptions
	opt.Name = msName
	opt.Dimensions = tc.dims
	opt.Ascending = ascending
	opt.FieldAux = tc.fieldAux
	opt.MaxParallel = 1
	opt.ChunkSize = chunkSize
	opt.StartTime = tc.startTime
	opt.EndTime = tc.endTime
	opt.Interval = hybridqp.Interval{
		Duration: interval,
	}

	addFilterFieldCondition(tc.fieldFilter, &opt)

	return &opt
}

func genCall(fieldAux []influxql.VarRef, call string) []influxql.Call {
	field := make([]influxql.Call, 0, len(fieldAux))
	for i := range fieldAux {
		f := influxql.Call{
			Name: call,
			Args: []influxql.Expr{
				&fieldAux[i],
			},
		}
		field = append(field, f)
	}
	return field
}

func genOps(vaRefs []influxql.VarRef, calls []influxql.Call) []hybridqp.ExprOptions {
	ops := make([]hybridqp.ExprOptions, 0, len(calls))
	for i := range calls {
		ops = append(ops, hybridqp.ExprOptions{
			Expr: &calls[i],
			Ref:  vaRefs[i],
		})
	}
	return ops
}

func updateClusterCursor(cursors comm.KeyCursors, ops []hybridqp.ExprOptions, call string) {
	for _, c := range cursors {
		var callOps []*comm.CallOption
		for _, op := range ops {
			if call, ok := op.Expr.(*influxql.Call); ok {
				callOps = append(callOps, &comm.CallOption{Call: call, Ref: &influxql.VarRef{
					Val:  op.Ref.Val,
					Type: op.Ref.Type,
				}})
			}
		}
		schema := c.(*groupCursor).ctx.schema
		clusterCursor := c.(*groupCursor).tagSetCursors[0].(*AggTagSetCursor)
		if call == "count" {
			schemaCopy := schema.Copy()
			for i := 0; i < len(schemaCopy)-1; i++ {
				schemaCopy[i].Type = influx.Field_Type_Int
			}
			clusterCursor.SetSchema(schemaCopy)
			clusterCursor.aggOps = callOps
			clusterCursor.initOpsFunctions()
			aggCursor := clusterCursor.baseCursorInfo.keyCursor.(*aggregateCursor)
			aggCursor.SetSchema(schema, schemaCopy, ops)
			aggCursor.input.(*fileLoopCursor).SetSchema(schema)
		} else {
			clusterCursor.SetSchema(schema)
			clusterCursor.aggOps = callOps
			clusterCursor.initOpsFunctions()
			aggCursor := clusterCursor.baseCursorInfo.keyCursor.(*aggregateCursor)
			aggCursor.SetSchema(schema, schema, ops)
			aggCursor.input.(*fileLoopCursor).SetSchema(schema)
		}
	}
}

type aggResult map[string][]interface{}

func GenAggDataRecord(msNames []string, seriesNum, pointNumOfPerSeries int, interval time.Duration,
	tm time.Time, fullField, inc bool, fixBool bool, tv ...int) ([]influx.Row, int64, int64, *aggResult) {
	result := aggResult{}
	result = make(map[string][]interface{})
	result["first"] = make([]interface{}, 4)
	result["last"] = make([]interface{}, 4)
	result["sum"] = make([]interface{}, 4)
	result["min"] = make([]interface{}, 4)
	result["max"] = make([]interface{}, 4)
	result["count"] = make([]interface{}, 4)
	tm = tm.Truncate(time.Second)
	pts := make([]influx.Row, 0, seriesNum)
	names := msNames
	if len(msNames) == 0 {
		names = []string{defaultMeasurementName}
	}

	mms := func(i int) string {
		return names[i%len(names)]
	}

	var indexKeyPool []byte
	vInt, vFloat := int64(1), float64(1)
	tv1, tv2, tv3, tv4 := 1, 1, 1, 1
	for _, tgv := range tv {
		tv1 = tgv
	}
	var (
		intSum       int64
		float64Sum   float64
		intCount     int64
		floatCount   int64
		stringCount  int64
		booleanCount int64
	)
	for i := 0; i < seriesNum; i++ {
		fields := map[string]interface{}{
			"field2_int":    vInt,
			"field3_bool":   i%2 == 0,
			"field4_float":  vFloat,
			"field1_string": fmt.Sprintf("test-test-test-test-%d", i),
		}
		if fixBool {
			fields["field3_bool"] = (i%2 == 0)
		} else {
			fields["field3_bool"] = (rand.Int31n(100) > 50)
		}

		if !fullField {
			if i%10 == 0 {
				delete(fields, "field1_string")
			}

			if i%25 == 0 {
				delete(fields, "field4_float")
			}

			if i%35 == 0 {
				delete(fields, "field3_bool")
			}
		}

		r := influx.Row{}

		// fields init
		r.Fields = make([]influx.Field, len(fields))
		j := 0
		for k, v := range fields {
			r.Fields[j].Key = k
			switch v.(type) {
			case int64:
				r.Fields[j].Type = influx.Field_Type_Int
				r.Fields[j].NumValue = float64(v.(int64))
				if result["min"][1] == nil || v.(int64) < result["min"][1].(int64) {
					result["min"][1] = v
				}
				if result["max"][1] == nil || v.(int64) > result["max"][1].(int64) {
					result["max"][1] = v
				}
				intSum += v.(int64)
				intCount += 1
			case float64:
				r.Fields[j].Type = influx.Field_Type_Float
				r.Fields[j].NumValue = v.(float64)
				if result["min"][3] == nil || v.(float64) < result["min"][3].(float64) {
					result["min"][3] = v
				}
				if result["max"][3] == nil || v.(float64) > result["max"][3].(float64) {
					result["max"][3] = v
				}
				float64Sum += r.Fields[j].NumValue
				floatCount += 1
			case string:
				r.Fields[j].Type = influx.Field_Type_String
				r.Fields[j].StrValue = v.(string)
				if result["first"][0] == nil || v.(string) > result["first"][0].(string) {
					result["first"][0] = v
				}
				stringCount += 1
			case bool:
				r.Fields[j].Type = influx.Field_Type_Boolean
				if v.(bool) == false {
					r.Fields[j].NumValue = 0
				} else {
					r.Fields[j].NumValue = 1
				}
				booleanCount += 1
			}
			j++
		}

		sort.Sort(r.Fields)

		vInt++
		vFloat += 1
		tags := map[string]string{
			"tagkey1": fmt.Sprintf("tagvalue1_%d", tv1),
			"tagkey2": fmt.Sprintf("tagvalue2_%d", tv2),
			"tagkey3": fmt.Sprintf("tagvalue3_%d", tv3),
			"tagkey4": fmt.Sprintf("tagvalue4_%d", tv4),
		}

		// tags init
		r.Tags = make(influx.PointTags, len(tags))
		j = 0
		for k, v := range tags {
			r.Tags[j].Key = k
			r.Tags[j].Value = v
			j++
		}
		sort.Sort(&r.Tags)
		tv4++
		tv3++
		tv2++
		tv1++

		name := mms(i)
		r.Name = name
		r.Timestamp = tm.UnixNano()
		r.UnmarshalIndexKeys(indexKeyPool)
		r.ShardKey = r.IndexKey

		pts = append(pts, r)
	}
	if pointNumOfPerSeries > 1 {
		copyRs := copyIntervalStepPointRows(pts, pointNumOfPerSeries-1, interval, inc)
		pts = append(pts, copyRs...)
	}

	sort.Slice(pts, func(i, j int) bool {
		return pts[i].Timestamp < pts[j].Timestamp
	})
	result["min"][0] = result["first"][0]
	result["min"][2] = false
	result["max"][0] = result["first"][0]
	result["max"][2] = true
	result["first"] = result["max"]
	result["last"] = result["max"]
	result["sum"] = []interface{}{nil, intSum, nil, float64Sum}
	result["count"] = []interface{}{stringCount, intCount, booleanCount, floatCount}
	return pts, pts[0].Timestamp, pts[len(pts)-1].Timestamp, &result
}
