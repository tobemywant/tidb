// Copyright 2022 PingCAP, Inc.
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

package ddl_test

import (
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/ddl/ingest"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAddIndexMergeProcess(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec("create table t (c1 int primary key, c2 int, c3 int)")
	tk.MustExec("insert into t values (1, 2, 3), (4, 5, 6);")
	// Force onCreateIndex use the txn-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")

	var checkErr error
	var runDML, backfillDone bool
	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{
		Do: dom,
	}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if !runDML && job.Type == model.ActionAddIndex && job.SchemaState == model.StateWriteReorganization {
			idx := findIdxInfo(dom, "test", "t", "idx")
			if idx == nil || idx.BackfillState != model.BackfillStateRunning {
				return
			}
			if !backfillDone {
				// Wait another round so that the backfill range is determined(1-4).
				backfillDone = true
				return
			}
			runDML = true
			// Write record 7 to the temporary index.
			_, checkErr = tk2.Exec("insert into t values (7, 8, 9);")
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	dom.DDL().SetHook(callback)
	tk.MustExec("alter table t add index idx(c1);")
	dom.DDL().SetHook(originHook)
	require.True(t, backfillDone)
	require.True(t, runDML)
	require.NoError(t, checkErr)
	tk.MustExec("admin check table t;")
	tk.MustQuery("select * from t use index (idx);").Check(testkit.Rows("1 2 3", "4 5 6", "7 8 9"))
	tk.MustQuery("select * from t ignore index (idx);").Check(testkit.Rows("1 2 3", "4 5 6", "7 8 9"))
}

func TestAddPrimaryKeyMergeProcess(t *testing.T) {
	// Disable auto schema reload.
	store, dom := testkit.CreateMockStoreAndDomainWithSchemaLease(t, 0)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec("create table t (c1 int, c2 int, c3 int)")
	tk.MustExec("insert into t values (1, 2, 3), (4, 5, 6);")
	// Force onCreateIndex use the backfill-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")

	var checkErr error
	var runDML, backfillDone bool
	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{
		Do: nil, // We'll reload the schema manually.

	}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if !runDML && job.Type == model.ActionAddPrimaryKey && job.SchemaState == model.StateWriteReorganization {
			idx := findIdxInfo(dom, "test", "t", "primary")
			if idx == nil || idx.BackfillState != model.BackfillStateRunning || job.SnapshotVer == 0 {
				return
			}
			if !backfillDone {
				// Wait another round so that the backfill process is finished, but
				// the info schema is not updated.
				backfillDone = true
				return
			}
			runDML = true
			// Add delete record 4 to the temporary index.
			_, checkErr = tk2.Exec("delete from t where c1 = 4;")
		}
		assert.NoError(t, dom.Reload())
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	dom.DDL().SetHook(callback)
	tk.MustExec("alter table t add primary key idx(c1);")
	dom.DDL().SetHook(originHook)
	require.True(t, backfillDone)
	require.True(t, runDML)
	require.NoError(t, checkErr)
	tk.MustExec("admin check table t;")
	tk.MustQuery("select * from t use index (primary);").Check(testkit.Rows("1 2 3"))
	tk.MustQuery("select * from t ignore index (primary);").Check(testkit.Rows("1 2 3"))
}

func TestAddIndexMergeVersionIndexValue(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec("create table t (c1 int);")
	// Force onCreateIndex use the txn-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")

	var checkErr error
	var runDML bool
	var tblID, idxID int64
	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{
		Do: dom,
	}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if !runDML && job.Type == model.ActionAddIndex && job.SchemaState == model.StateWriteReorganization {
			idx := findIdxInfo(dom, "test", "t", "idx")
			if idx == nil || idx.BackfillState != model.BackfillStateReadyToMerge {
				return
			}
			runDML = true
			tblID = job.TableID
			idxID = idx.ID
			_, checkErr = tk2.Exec("insert into t values (1);")
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	dom.DDL().SetHook(callback)
	tk.MustExec("alter table t add unique index idx(c1);")
	dom.DDL().SetHook(originHook)
	require.True(t, runDML)
	require.NoError(t, checkErr)
	tk.MustExec("admin check table t;")
	tk.MustQuery("select * from t use index (idx);").Check(testkit.Rows("1"))
	tk.MustQuery("select * from t ignore index (idx);").Check(testkit.Rows("1"))

	snap := store.GetSnapshot(kv.MaxVersion)
	iter, err := snap.Iter(tablecodec.GetTableIndexKeyRange(tblID, idxID))
	require.NoError(t, err)
	require.True(t, iter.Valid())
	// The origin index value should not have 'm' version appended.
	require.Equal(t, []byte{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1}, iter.Value())
}

func TestAddIndexMergeIndexUntouchedValue(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec(`create table t (
    	id int not null auto_increment,
		k int not null default '0',
		c char(120) not null default '',
		pad char(60) not null default '',
		primary key (id) clustered,
		key k_1(k));`)
	tk.MustExec("insert into t values (1, 1, 'a', 'a')")
	// Force onCreateIndex use the txn-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")

	var checkErrs []error
	var runInsert bool
	var runUpdate bool
	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{
		Do: dom,
	}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if job.Type != model.ActionAddIndex || job.SchemaState != model.StateWriteReorganization {
			return
		}
		idx := findIdxInfo(dom, "test", "t", "idx")
		if idx == nil {
			return
		}
		if !runInsert {
			if idx.BackfillState != model.BackfillStateRunning || job.SnapshotVer == 0 {
				return
			}
			runInsert = true
			_, err := tk2.Exec("insert into t values (100, 1, 'a', 'a');")
			checkErrs = append(checkErrs, err)
		}
		if !runUpdate {
			if idx.BackfillState != model.BackfillStateReadyToMerge {
				return
			}
			runUpdate = true
			_, err := tk2.Exec("begin;")
			checkErrs = append(checkErrs, err)
			_, err = tk2.Exec("update t set k=k+1 where id = 100;")
			checkErrs = append(checkErrs, err)
			_, err = tk2.Exec("commit;")
			checkErrs = append(checkErrs, err)
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	dom.DDL().SetHook(callback)
	tk.MustExec("alter table t add index idx(c);")
	dom.DDL().SetHook(originHook)
	require.True(t, runUpdate)
	for _, err := range checkErrs {
		require.NoError(t, err)
	}
	tk.MustExec("admin check table t;")
	tk.MustQuery("select * from t use index (idx);").Check(testkit.Rows("1 1 a a", "100 2 a a"))
	tk.MustQuery("select * from t ignore index (idx);").Check(testkit.Rows("1 1 a a", "100 2 a a"))
}

func findIdxInfo(dom *domain.Domain, dbName, tbName, idxName string) *model.IndexInfo {
	tbl, err := dom.InfoSchema().TableByName(model.NewCIStr(dbName), model.NewCIStr(tbName))
	if err != nil {
		logutil.BgLogger().Warn("cannot find table", zap.String("dbName", dbName), zap.String("tbName", tbName))
		return nil
	}
	return tbl.Meta().FindIndexByName(idxName)
}

// TestCreateUniqueIndexKeyExist this case will test below things:
// Create one unique index idx((a*b+1));
// insert (0, 6) and delete it;
// insert (0, 9), it should be successful;
// Should check temp key exist and skip deleted mark
// The error returned below:
// Error:      	Received unexpected error:
//
//	[kv:1062]Duplicate entry '1' for key 't.idx'
func TestCreateUniqueIndexKeyExist(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t(a int default 0, b int default 0)")
	tk.MustExec("insert into t values (1, 1), (2, 2), (3, 3), (4, 4)")

	tk1 := testkit.NewTestKit(t, store)
	tk1.MustExec("use test")

	stateDeleteOnlySQLs := []string{"insert into t values (5, 5)", "begin pessimistic;", "insert into t select * from t", "rollback", "insert into t set b = 6", "update t set b = 7 where a = 1", "delete from t where b = 4"}

	// If waitReorg timeout, the worker may enter writeReorg more than 2 times.
	reorgTime := 0
	d := dom.DDL()
	originalCallback := d.GetHook()
	defer d.SetHook(originalCallback)
	callback := &ddl.TestDDLCallback{}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if t.Failed() {
			return
		}
		var err error
		switch job.SchemaState {
		case model.StateDeleteOnly:
			for _, sql := range stateDeleteOnlySQLs {
				_, err = tk1.Exec(sql)
				assert.NoError(t, err)
			}
			// (1, 7), (2, 2), (3, 3), (5, 5), (0, 6)
		case model.StateWriteOnly:
			_, err = tk1.Exec("insert into t values (8, 8)")
			assert.NoError(t, err)
			_, err = tk1.Exec("update t set b = 7 where a = 2")
			assert.NoError(t, err)
			_, err = tk1.Exec("delete from t where b = 3")
			assert.NoError(t, err)
			// (1, 7), (2, 7), (5, 5), (0, 6), (8, 8)
		case model.StateWriteReorganization:
			if reorgTime < 1 {
				reorgTime++
			} else {
				return
			}
			_, err = tk1.Exec("insert into t values (10, 10)")
			assert.NoError(t, err)
			_, err = tk1.Exec("delete from t where b = 6")
			assert.NoError(t, err)
			_, err = tk1.Exec("insert into t set b = 9")
			assert.NoError(t, err)
			_, err = tk1.Exec("update t set b = 7 where a = 5")
			assert.NoError(t, err)
			// (1, 7), (2, 7), (5, 7), (8, 8), (10, 10), (0, 9)
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	d.SetHook(callback)
	tk.MustExec("alter table t add unique index idx((a*b+1))")
	tk.MustExec("admin check table t")
	tk.MustQuery("select * from t order by a, b").Check(testkit.Rows("0 9", "1 7", "2 7", "5 7", "8 8", "10 10"))
}

func TestAddIndexMergeIndexUpdateOnDeleteOnly(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec(`CREATE TABLE t (a DATE NULL DEFAULT '1619-01-18', b BOOL NULL DEFAULT '0') CHARACTER SET 'utf8mb4' COLLATE 'utf8mb4_bin';`)
	tk.MustExec(`INSERT INTO t SET b = '1';`)

	updateSQLs := []string{
		"UPDATE t SET a = '9432-05-10', b = '0';",
		"UPDATE t SET a = '9432-05-10', b = '1';",
	}

	// Force onCreateIndex use the txn-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")
	tk.MustExec("set @@global.tidb_enable_mutation_checker = 1;")
	tk.MustExec("set @@global.tidb_txn_assertion_level = 'STRICT';")

	var checkErrs []error
	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{
		Do: dom,
	}
	onJobUpdatedBefore := func(job *model.Job) {
		if job.SchemaState == model.StateDeleteOnly {
			for _, sql := range updateSQLs {
				_, err := tk2.Exec(sql)
				if err != nil {
					checkErrs = append(checkErrs, err)
				}
			}
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedBefore)
	dom.DDL().SetHook(callback)
	tk.MustExec("alter table t add index idx(b);")
	dom.DDL().SetHook(originHook)
	for _, err := range checkErrs {
		require.NoError(t, err)
	}
	tk.MustExec("admin check table t;")
}

func TestAddIndexMergeDeleteUniqueOnWriteOnly(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t(a int default 0, b int default 0);")
	tk.MustExec("insert into t values (1, 1), (2, 2), (3, 3), (4, 4);")

	tk1 := testkit.NewTestKit(t, store)
	tk1.MustExec("use test")

	d := dom.DDL()
	originalCallback := d.GetHook()
	defer d.SetHook(originalCallback)
	callback := &ddl.TestDDLCallback{}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if t.Failed() {
			return
		}
		var err error
		switch job.SchemaState {
		case model.StateDeleteOnly:
			_, err = tk1.Exec("insert into t values (5, 5);")
			assert.NoError(t, err)
		case model.StateWriteOnly:
			_, err = tk1.Exec("insert into t values (5, 7);")
			assert.NoError(t, err)
			_, err = tk1.Exec("delete from t where b = 7;")
			assert.NoError(t, err)
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	d.SetHook(callback)
	tk.MustExec("alter table t add unique index idx(a);")
	tk.MustExec("admin check table t;")
}

func TestAddIndexMergeDeleteNullUnique(t *testing.T) {
	store := testkit.CreateMockStore(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t(id int primary key, a int default 0);")
	tk.MustExec("insert into t values (1, 1), (2, null);")

	tk1 := testkit.NewTestKit(t, store)
	tk1.MustExec("use test")

	ddl.MockDMLExecution = func() {
		_, err := tk1.Exec("delete from t where id = 2;")
		assert.NoError(t, err)
	}
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/ddl/mockDMLExecution", "1*return(true)->return(false)"))
	tk.MustExec("alter table t add unique index idx(a);")
	tk.MustQuery("select count(1) from t;").Check(testkit.Rows("1"))
	tk.MustExec("admin check table t;")
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/ddl/mockDMLExecution"))
}

func TestAddIndexMergeDoubleDelete(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t(id int primary key, a int default 0);")

	tk1 := testkit.NewTestKit(t, store)
	tk1.MustExec("use test")

	d := dom.DDL()
	originalCallback := d.GetHook()
	defer d.SetHook(originalCallback)
	callback := &ddl.TestDDLCallback{}
	onJobUpdatedExportedFunc := func(job *model.Job) {
		if t.Failed() {
			return
		}
		switch job.SchemaState {
		case model.StateWriteOnly:
			_, err := tk1.Exec("insert into t values (1, 1);")
			assert.NoError(t, err)
		}
	}
	callback.OnJobUpdatedExported.Store(&onJobUpdatedExportedFunc)
	d.SetHook(callback)

	ddl.MockDMLExecution = func() {
		_, err := tk1.Exec("delete from t where id = 1;")
		assert.NoError(t, err)
		_, err = tk1.Exec("insert into t values (2, 1);")
		assert.NoError(t, err)
		_, err = tk1.Exec("delete from t where id = 2;")
		assert.NoError(t, err)
	}
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/ddl/mockDMLExecution", "1*return(true)->return(false)"))
	tk.MustExec("alter table t add unique index idx(a);")
	tk.MustQuery("select count(1) from t;").Check(testkit.Rows("0"))
	tk.MustExec("admin check table t;")
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/ddl/mockDMLExecution"))
}

func TestAddIndexMergeConflictWithPessimistic(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec(`CREATE TABLE t (id int primary key, a int);`)
	tk.MustExec(`INSERT INTO t VALUES (1, 1);`)

	// Force onCreateIndex use the txn-merge process.
	ingest.LitInitialized = false
	tk.MustExec("set @@global.tidb_ddl_enable_fast_reorg = 1;")
	tk.MustExec("set @@global.tidb_enable_metadata_lock = 0;")

	originHook := dom.DDL().GetHook()
	callback := &ddl.TestDDLCallback{Do: dom}

	runPessimisticTxn := false
	callback.OnJobRunBeforeExported = func(job *model.Job) {
		if t.Failed() {
			return
		}
		if job.SchemaState == model.StateWriteOnly {
			// Write a record to the temp index.
			_, err := tk2.Exec("update t set a = 2 where id = 1;")
			assert.NoError(t, err)
		}
		if !runPessimisticTxn && job.SchemaState == model.StateWriteReorganization {
			idx := findIdxInfo(dom, "test", "t", "idx")
			if idx == nil {
				return
			}
			if idx.BackfillState != model.BackfillStateReadyToMerge {
				return
			}
			runPessimisticTxn = true
			_, err := tk2.Exec("begin pessimistic;")
			assert.NoError(t, err)
			_, err = tk2.Exec("update t set a = 3 where id = 1;")
			assert.NoError(t, err)
		}
	}
	dom.DDL().SetHook(callback)
	afterCommit := make(chan struct{}, 1)
	go func() {
		tk.MustExec("alter table t add index idx(a);")
		afterCommit <- struct{}{}
	}()
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case <-timer.C:
		break
	case <-afterCommit:
		require.Fail(t, "should be blocked by the pessimistic txn")
	}
	tk2.MustExec("rollback;")
	<-afterCommit
	dom.DDL().SetHook(originHook)
	tk.MustExec("admin check table t;")
	tk.MustQuery("select * from t;").Check(testkit.Rows("1 2"))
}
