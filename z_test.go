// Copyright 2020 Tamás Gulácsi
//
//
// SPDX-License-Identifier: UPL-1.0 OR Apache-2.0

package godror_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logfmt/logfmt"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/sync/errgroup"

	godror "github.com/godror/godror"
)

var (
	testDb       *sql.DB
	testSystemDb *sql.DB
	tl           = &testLogger{}

	clientVersion, serverVersion godror.VersionInfo
	testConStr                   string
	testSystemConStr             string
)

var tblSuffix string

const maxSessions = 16
const useDefaultFetchValue = -99

func init() {
	hsh := fnv.New32()
	hsh.Write([]byte(runtime.Version()))
	tblSuffix = fmt.Sprintf("_%x", hsh.Sum(nil))

	godror.Log = func(...interface{}) error { return nil }
	if b, _ := strconv.ParseBool(os.Getenv("VERBOSE")); b {
		tl.enc = logfmt.NewEncoder(os.Stderr)
		godror.Log = tl.Log
	}
	if tzName := os.Getenv("GODROR_TIMEZONE"); tzName != "" {
		var err error
		if time.Local, err = time.LoadLocation(tzName); err != nil {
			panic(fmt.Errorf("unknown GODROR_TIMEZONE=%q: %w", tzName, err))
		}
	}

	eUsername, ePassword, eDB := os.Getenv("GODROR_TEST_USERNAME"), os.Getenv("GODROR_TEST_PASSWORD"), os.Getenv("GODROR_TEST_DB")
	var configDir string
	if eUsername == "" && (eDB == "" || os.Getenv("TNS_ADMIN") == "") {
		wd, err := os.Getwd()
		if err != nil {
			panic(err)
		}
		wd = filepath.Join(wd, "contrib", "free.db")
		tempDir, err := ioutil.TempDir("", "godror_drv_test-")
		if err != nil {
			panic(err)
		}
		// defer os.RemoveAll(tempDir)
		for _, nm := range []string{"tnsnames.ora", "cwallet.sso", "ewallet.p12"} {
			var sfh *os.File
			if sfh, err = os.Open(filepath.Join(wd, nm)); err != nil {
				panic(err)
			}
			dfh, err := os.Create(filepath.Join(tempDir, nm))
			if err != nil {
				sfh.Close()
				panic(err)
			}
			_, err = io.Copy(dfh, sfh)
			sfh.Close()
			dfh.Close()
			if err != nil {
				panic(err)
			}
		}
		b, err := ioutil.ReadFile(filepath.Join(wd, "sqlnet.ora"))
		if err != nil {
			panic(err)
		}
		if err = ioutil.WriteFile(
			filepath.Join(tempDir, "sqlnet.ora"),
			bytes.Replace(b,
				[]byte(`DIRECTORY="?/network/admin"`),
				[]byte(`DIRECTORY="`+wd+`"`), 1),
			0644,
		); err != nil {
			panic(err)
		}

		fn := filepath.Join(wd, "env.sh")
		fmt.Println("Using default database for tests: ", fn)
		fmt.Printf("export TNS_ADMIN=%q\n", wd)
		os.Setenv("TNS_ADMIN", tempDir)
		configDir = tempDir

		if b, err = ioutil.ReadFile(fn); err != nil {
			fmt.Println(err)
		} else {
			const prefix = "export GODROR_TEST_"
			for _, line := range bytes.Split(b, []byte{'\n'}) {
				if !bytes.HasPrefix(line, []byte(prefix)) {
					continue
				}
				line = line[len(prefix):]
				i := bytes.IndexByte(line, '=')
				if i < 0 {
					continue
				}
				k, v := string(line[:i]), string(line[i+1:])
				os.Setenv("GODROR_TEST_"+k, v)
				switch k {
				case "USERNAME":
					eUsername = v
				case "PASSWORD":
					ePassword = v
				case "DB":
					eDB = v
				}
			}
		}
	}

	P := godror.ConnectionParams{
		CommonParams: godror.CommonParams{
			Username:      eUsername,
			Password:      godror.NewPassword(ePassword),
			ConnectString: eDB,
			EnableEvents:  true,
			ConfigDir:     configDir,
		},
		ConnParams: godror.ConnParams{
			ConnClass:   "TestClassName",
			ShardingKey: []interface{}{"gold", []byte("silver"), int(42)},
		},
		PoolParams: godror.PoolParams{
			MinSessions: 2, MaxSessions: maxSessions, SessionIncrement: 2,
			WaitTimeout:    5 * time.Second,
			MaxLifeTime:    5 * time.Minute,
			SessionTimeout: 1 * time.Minute,
		},
		StandaloneConnection: godror.DefaultStandaloneConnection,
	}
	for _, k := range []string{"USERNAME", "PASSWORD", "DB", "STANDALONE"} {
		k = "GODROR_TEST_" + k
		fmt.Printf("export %q=%q\n", k, os.Getenv(k))
	}
	if b, err := strconv.ParseBool(os.Getenv("GODROR_TEST_STANDALONE")); err == nil {
		P.StandaloneConnection = b
	} else {
		fmt.Printf("# GODROR_TEST_STANDALONE is not set, using default %t\n", godror.DefaultStandaloneConnection)
	}
	if strings.HasSuffix(strings.ToUpper(P.Username), " AS SYSDBA") {
		P.IsSysDBA, P.Username = true, P.Username[:len(P.Username)-10]
	}
	testConStr = P.StringWithPassword()
	if eSysUsername, eSysPassword := os.Getenv("GODROR_TEST_SYSTEM_USERNAME"), os.Getenv("GODROR_TEST_SYSTEM_PASSWORD"); eSysUsername != "" && eSysPassword != "" {
		PSystem := P
		PSystem.Username, PSystem.Password = eSysUsername, godror.NewPassword(eSysPassword)
		testSystemConStr = PSystem.StringWithPassword()
	}
	var err error
	if testDb, err = sql.Open("godror", testConStr); err != nil {
		panic(fmt.Errorf("%s: %+v", testConStr, err))
	}

	fmt.Println("#", P.String())
	fmt.Println("Version:", godror.Version)
	ctx, cancel := context.WithTimeout(testContext("init"), 30*time.Second)
	defer cancel()
	if err = godror.Raw(ctx, testDb, func(cx godror.Conn) error {
		if clientVersion, err = cx.ClientVersion(); err != nil {
			return err
		}
		fmt.Println("Client:", clientVersion, "Timezone:", time.Local.String())
		if serverVersion, err = cx.ServerVersion(); err != nil {
			return err
		}
		dbTZ := cx.Timezone()
		fmt.Println("Server:", serverVersion, "Timezone:", dbTZ.String())
		return nil
	}); err != nil {
		panic(err)
	}

	go func() {
		for range time.NewTicker(time.Second).C {
			runtime.GC()
		}
	}()

	statTicker = make(chan time.Time)
	ticks := time.NewTicker(30 * time.Second).C
	go func() {
		for t := range ticks {
			statTicker <- t
		}
	}()

	if P.StandaloneConnection {
		testDb.SetMaxIdleConns(maxSessions / 2)
		testDb.SetMaxOpenConns(maxSessions)
		testDb.SetConnMaxLifetime(10 * time.Minute)
		go func() {
			for range statTicker {
				fmt.Printf("testDb: %+v\n", testDb.Stats())
			}
		}()
	} else {
		// Disable Go db connection pooling
		testDb.SetMaxIdleConns(0)
		testDb.SetMaxOpenConns(0)
		testDb.SetConnMaxLifetime(0)
		go func() {
			for range statTicker {
				ctx, cancel := context.WithTimeout(testContext("poolStats"), time.Second)
				godror.Raw(ctx, testDb, func(c godror.Conn) error {
					poolStats, err := c.GetPoolStats()
					fmt.Printf("testDb: %s %v\n", poolStats, err)
					return err
				})
				cancel()
			}
		}()
	}
}

var statTicker chan time.Time

func PrintConnStats() {
	statTicker <- time.Now()
}

func testContext(name string) context.Context {
	return godror.ContextWithTraceTag(context.Background(), godror.TraceTag{Module: "Test" + name})
}

var bufPool = sync.Pool{New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 1024)) }}

type testLogger struct {
	mu       sync.RWMutex
	enc      *logfmt.Encoder
	Ts       []*testing.T
	beHelped []*testing.T
}

func (tl *testLogger) Log(args ...interface{}) error {
	if tl.enc != nil {
		for i := 1; i < len(args); i += 2 {
			switch args[i].(type) {
			case string, fmt.Stringer:
			default:
				args[i] = fmt.Sprintf("%+v", args[i])
			}
		}
		tl.mu.Lock()
		tl.enc.Reset()
		tl.enc.EncodeKeyvals(args...)
		tl.enc.EndRecord()
		tl.mu.Unlock()
	}
	return tl.GetLog()(args)
}
func (tl *testLogger) GetLog() func(keyvals ...interface{}) error {
	return func(keyvals ...interface{}) error {
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		if len(keyvals)%2 != 0 {
			keyvals = append(append(make([]interface{}, 0, len(keyvals)+1), "msg"), keyvals...)
		}
		for i := 0; i < len(keyvals); i += 2 {
			fmt.Fprintf(buf, "%s=%#v ", keyvals[i], keyvals[i+1])
		}

		tl.mu.Lock()
		for _, t := range tl.beHelped {
			t.Helper()
		}
		tl.beHelped = tl.beHelped[:0]
		tl.mu.Unlock()

		tl.mu.RLock()
		defer tl.mu.RUnlock()
		for _, t := range tl.Ts {
			t.Helper()
			t.Log(buf.String())
		}

		return nil
	}
}
func (tl *testLogger) enableLogging(t *testing.T) func() {
	tl.mu.Lock()
	tl.Ts = append(tl.Ts, t)
	tl.beHelped = append(tl.beHelped, t)
	tl.mu.Unlock()

	return func() {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		for i, f := range tl.Ts {
			if f == t {
				tl.Ts[i] = tl.Ts[0]
				tl.Ts = tl.Ts[1:]
				break
			}
		}
		for i, f := range tl.beHelped {
			if f == t {
				tl.beHelped[i] = tl.beHelped[0]
				tl.beHelped = tl.beHelped[1:]
				break
			}
		}
	}
}

func TestDescribeQuery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("DescribeQuery"), 10*time.Second)
	defer cancel()

	const qry = "SELECT * FROM user_tab_cols"
	cols, err := godror.DescribeQuery(ctx, testDb, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	t.Log(cols)
}

func TestParseOnly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("ParseOnly"), 10*time.Second)
	defer cancel()

	tbl := "test_not_exist" + tblSuffix
	cnt := func() int {
		var cnt int64
		if err := testDb.QueryRowContext(ctx,
			"SELECT COUNT(0) FROM user_tables WHERE table_name = UPPER('"+tbl+"')").Scan(&cnt); //nolint:gas
		err != nil {
			t.Fatal(err)
		}
		return int(cnt)
	}

	if cnt() != 0 {
		if _, err := testDb.ExecContext(ctx, "DROP TABLE "+tbl); err != nil {
			t.Error(err)
		}
	}
	if _, err := testDb.ExecContext(ctx, "CREATE TABLE "+tbl+"(t VARCHAR2(1))", godror.ParseOnly()); err != nil {
		t.Fatal(err)
	}
	if got := cnt(); got != 1 {
		t.Errorf("got %d, wanted 0", got)
	}
}

func TestInputArray(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("InputArray"), 10*time.Second)
	defer cancel()

	pkg := strings.ToUpper("test_in_pkg" + tblSuffix)
	qry := `CREATE OR REPLACE PACKAGE ` + pkg + ` AS
TYPE int_tab_typ IS TABLE OF BINARY_INTEGER INDEX BY PLS_INTEGER;
TYPE num_tab_typ IS TABLE OF NUMBER INDEX BY PLS_INTEGER;
TYPE vc_tab_typ IS TABLE OF VARCHAR2(100) INDEX BY PLS_INTEGER;
TYPE dt_tab_typ IS TABLE OF DATE INDEX BY PLS_INTEGER;
TYPE ids_tab_typ IS TABLE OF INTERVAL DAY TO SECOND INDEX BY PLS_INTEGER;
--TYPE lob_tab_typ IS TABLE OF CLOB INDEX BY PLS_INTEGER;

FUNCTION in_int(p_int IN int_tab_typ) RETURN VARCHAR2;
FUNCTION in_num(p_num IN num_tab_typ) RETURN VARCHAR2;
FUNCTION in_vc(p_vc IN vc_tab_typ) RETURN VARCHAR2;
FUNCTION in_dt(p_dt IN dt_tab_typ) RETURN VARCHAR2;
FUNCTION in_ids(p_dur IN ids_tab_typ) RETURN VARCHAR2;
END;
`
	testDb.Exec("DROP PACKAGE " + pkg)
	t.Log("package", pkg)
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	defer testDb.Exec("DROP PACKAGE " + pkg)

	qry = `CREATE OR REPLACE PACKAGE BODY ` + pkg + ` AS
FUNCTION in_int(p_int IN int_tab_typ) RETURN VARCHAR2 IS
  v_idx PLS_INTEGER;
  v_res VARCHAR2(32767);
BEGIN
  v_idx := p_int.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    v_res := v_res||v_idx||':'||p_int(v_idx)||CHR(10);
    v_idx := p_int.NEXT(v_idx);
  END LOOP;
  RETURN(v_res);
END;

FUNCTION in_num(p_num IN num_tab_typ) RETURN VARCHAR2 IS
  v_idx PLS_INTEGER;
  v_res VARCHAR2(32767);
BEGIN
  v_idx := p_num.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    v_res := v_res||v_idx||':'||p_num(v_idx)||CHR(10);
    v_idx := p_num.NEXT(v_idx);
  END LOOP;
  RETURN(v_res);
END;

FUNCTION in_vc(p_vc IN vc_tab_typ) RETURN VARCHAR2 IS
  v_idx PLS_INTEGER;
  v_res VARCHAR2(32767);
BEGIN
  v_idx := p_vc.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    v_res := v_res||v_idx||':'||p_vc(v_idx)||CHR(10);
    v_idx := p_vc.NEXT(v_idx);
  END LOOP;
  RETURN(v_res);
END;
FUNCTION in_dt(p_dt IN dt_tab_typ) RETURN VARCHAR2 IS
  v_idx PLS_INTEGER;
  v_res VARCHAR2(32767);
BEGIN
  v_idx := p_dt.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    v_res := v_res||v_idx||':'||TO_CHAR(p_dt(v_idx), 'YYYY-MM-DD"T"HH24:MI:SS')||CHR(10);
    v_idx := p_dt.NEXT(v_idx);
  END LOOP;
  RETURN(v_res);
END;
FUNCTION in_ids(p_dur IN ids_tab_typ) RETURN VARCHAR2 IS
  V_idx PLS_INTEGER;
  v_res VARCHAR2(32767);
BEGIN
  v_idx := p_dur.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    v_res := v_res||v_idx||':'||TO_CHAR(p_dur(v_idx), 'YYYY-MM-DD"T"HH24:MI:SS')||CHR(10);
    v_idx := p_dur.NEXT(v_idx);
  END LOOP;
  RETURN(v_res);
END;
END;
`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	compileErrors, err := godror.GetCompileErrors(testDb, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(compileErrors) != 0 {
		t.Logf("compile errors: %v", compileErrors)
		for _, ce := range compileErrors {
			if strings.Contains(ce.Error(), pkg) {
				t.Fatal(ce)
			}
		}
	}
	serverTZ := time.Local
	if err = godror.Raw(ctx, testDb, func(conn godror.Conn) error {
		serverTZ = conn.Timezone()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	tx, err := testDb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	qry = "ALTER SESSION SET time_zone = 'UTC'"
	if _, err = tx.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}

	epoch := time.Date(2017, 11, 20, 12, 14, 21, 0, time.UTC)
	epochPlus := epoch.AddDate(0, -6, 0)
	const timeFmt = "2006-01-02T15:04:05"
	_ = epoch
	for name, tC := range map[string]struct {
		In   interface{}
		Want string
	}{
		// "int_0":{In:[]int32{}, Want:""},
		"num_0": {In: []godror.Number{}, Want: ""},
		"vc_0":  {In: []string{}, Want: ""},
		"dt_0":  {In: []time.Time{}, Want: ""},
		"dt_00": {In: []godror.NullTime{}, Want: ""},

		"num_3": {
			In:   []godror.Number{"1", "2.72", "-3.14"},
			Want: "1:1\n2:2.72\n3:-3.14\n",
		},
		"vc_3": {
			In:   []string{"a", "", "cCc"},
			Want: "1:a\n2:\n3:cCc\n",
		},
		"dt_2": {
			In: []time.Time{epoch, epochPlus},
			Want: ("1:" + epoch.In(serverTZ).Format(timeFmt) + "\n" +
				"2:" + epochPlus.In(serverTZ).Format(timeFmt) + "\n"),
		},
		"dt_02": {
			In: []godror.NullTime{{Valid: true, Time: epoch},
				{Valid: true, Time: epochPlus}},
			Want: ("1:" + epoch.In(serverTZ).Format(timeFmt) + "\n" +
				"2:" + epochPlus.In(serverTZ).Format(timeFmt) + "\n"),
		},

		// "ids_1": { In:   []time.Duration{32 * time.Second}, Want: "1:32s\n", },
	} {
		typ := strings.SplitN(name, "_", 2)[0]
		qry := "BEGIN :1 := " + pkg + ".in_" + typ + "(:2); END;"
		var res string
		if _, err := tx.ExecContext(ctx, qry, godror.PlSQLArrays,
			sql.Out{Dest: &res}, tC.In,
		); err != nil {
			t.Error(fmt.Errorf("%q. %s %+v: %w", name, qry, tC.In, err))
		}
		t.Logf("%q. %q", name, res)
		if typ == "num" {
			res = strings.Replace(res, ",", ".", -1)
		}
		if res != tC.Want {
			t.Errorf("%q. got %q, wanted %q.", name, res, tC.Want)
		}
	}
}

func TestDbmsOutput(t *testing.T) {
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("DbmsOutput"), 10*time.Second)
	defer cancel()

	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := godror.EnableDbmsOutput(ctx, conn); err != nil {
		t.Fatal(err)
	}

	txt := `árvíztűrő tükörfúrógép`
	qry := "BEGIN DBMS_OUTPUT.PUT_LINE('" + txt + "'); END;"
	if _, err := conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := godror.ReadDbmsOutput(ctx, &buf, conn); err != nil {
		t.Error(err)
	}
	t.Log(buf.String())
	if buf.String() != txt+"\n" {
		t.Errorf("got %q, wanted %q", buf.String(), txt+"\n")
	}
}

func TestInOutArray(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()

	ctx, cancel := context.WithTimeout(testContext("InOutArray"), 20*time.Second)
	defer cancel()

	pkg := strings.ToUpper("test_pkg" + tblSuffix)
	qry := `CREATE OR REPLACE PACKAGE ` + pkg + ` AS
TYPE int_tab_typ IS TABLE OF BINARY_INTEGER INDEX BY PLS_INTEGER;
TYPE num_tab_typ IS TABLE OF NUMBER INDEX BY PLS_INTEGER;
TYPE vc_tab_typ IS TABLE OF VARCHAR2(100) INDEX BY PLS_INTEGER;
TYPE dt_tab_typ IS TABLE OF DATE INDEX BY PLS_INTEGER;
TYPE lob_tab_typ IS TABLE OF CLOB INDEX BY PLS_INTEGER;

PROCEDURE inout_int(p_int IN OUT int_tab_typ);
PROCEDURE inout_num(p_num IN OUT num_tab_typ);
PROCEDURE inout_vc(p_vc IN OUT vc_tab_typ);
PROCEDURE inout_dt(p_dt IN OUT dt_tab_typ);
PROCEDURE p2(
	--p_int IN OUT int_tab_typ,
	p_num IN OUT num_tab_typ, p_vc IN OUT vc_tab_typ, p_dt IN OUT dt_tab_typ);
END;
`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	defer testDb.Exec("DROP PACKAGE " + pkg)

	qry = `CREATE OR REPLACE PACKAGE BODY ` + pkg + ` AS
PROCEDURE inout_int(p_int IN OUT int_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_int.COUNT='||p_int.COUNT||' FIRST='||p_int.FIRST||' LAST='||p_int.LAST);
  v_idx := p_int.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_int(v_idx) := NVL(p_int(v_idx) * 2, 1);
	v_idx := p_int.NEXT(v_idx);
  END LOOP;
  p_int(NVL(p_int.LAST, 0)+1) := p_int.COUNT;
END;

PROCEDURE inout_num(p_num IN OUT num_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_num.COUNT='||p_num.COUNT||' FIRST='||p_num.FIRST||' LAST='||p_num.LAST);
  v_idx := p_num.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_num(v_idx) := NVL(p_num(v_idx) / 2, 0.5);
	v_idx := p_num.NEXT(v_idx);
  END LOOP;
  p_num(NVL(p_num.LAST, 0)+1) := p_num.COUNT;
END;

PROCEDURE inout_vc(p_vc IN OUT vc_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_vc.COUNT='||p_vc.COUNT||' FIRST='||p_vc.FIRST||' LAST='||p_vc.LAST);
  v_idx := p_vc.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_vc(v_idx) := NVL(p_vc(v_idx) ||' +', '-');
	v_idx := p_vc.NEXT(v_idx);
  END LOOP;
  p_vc(NVL(p_vc.LAST, 0)+1) := p_vc.COUNT;
END;

PROCEDURE inout_dt(p_dt IN OUT dt_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_dt.COUNT='||p_dt.COUNT||' FIRST='||p_dt.FIRST||' LAST='||p_dt.LAST);
  v_idx := p_dt.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    DBMS_OUTPUT.PUT_LINE(v_idx||'='||TO_CHAR(p_dt(v_idx), 'YYYY-MM-DD HH24:MI:SS'));
    p_dt(v_idx) := NVL(p_dt(v_idx) + 1, TRUNC(SYSDATE)-v_idx);
	v_idx := p_dt.NEXT(v_idx);
  END LOOP;
  p_dt(NVL(p_dt.LAST, 0)+1) := TRUNC(SYSDATE);
  DBMS_OUTPUT.PUT_LINE('p_dt.COUNT='||p_dt.COUNT||' FIRST='||p_dt.FIRST||' LAST='||p_dt.LAST);
END;

PROCEDURE p2(
	--p_int IN OUT int_tab_typ,
	p_num IN OUT num_tab_typ,
	p_vc IN OUT vc_tab_typ,
	p_dt IN OUT dt_tab_typ
--, p_lob IN OUT lob_tab_typ
) IS
BEGIN
  --inout_int(p_int);
  inout_num(p_num);
  inout_vc(p_vc);
  inout_dt(p_dt);
  --p_lob := NULL;
END p2;
END;
`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	compileErrors, err := godror.GetCompileErrors(testDb, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(compileErrors) != 0 {
		t.Logf("compile errors: %v", compileErrors)
		for _, ce := range compileErrors {
			if strings.Contains(ce.Error(), pkg) {
				t.Fatal(ce)
			}
		}
	}

	intgr := []int32{3, 1, 4, 0, 0}[:3]
	intgrWant := []int32{3 * 2, 1 * 2, 4 * 2, 3}
	_ = intgrWant
	num := []godror.Number{"3.14", "-2.48", ""}[:2]
	numWant := []godror.Number{"1.57", "-1.24", "2"}
	vc := []string{"string", "bring", ""}[:2]
	vcWant := []string{"string +", "bring +", "2"}
	var today time.Time
	qry = "SELECT TRUNC(SYSDATE) FROM DUAL"
	if testDb.QueryRowContext(ctx, qry).Scan(&today); err != nil {
		t.Fatal(err)
	}
	dt := []time.Time{
		time.Date(2017, 6, 18, 7, 5, 51, 0, time.Local),
		{},
		today.Add(-2 * 24 * time.Hour),
		today,
	}
	dt[1] = dt[0].Add(24 * time.Hour)
	dtWant := make([]time.Time, len(dt))
	for i, d := range dt {
		if i < len(dt)-1 {
			// p_dt(v_idx) := NVL(p_dt(v_idx) + 1, TRUNC(SYSDATE)-v_idx);
			dtWant[i] = d.AddDate(0, 0, 1)
		} else {
			// p_dt(NVL(p_dt.LAST, 0)+1) := TRUNC(SYSDATE);
			dtWant[i] = d
		}
	}
	dt = dt[:len(dt)-1]

	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ALTER SESSION SET time_zone = local"); err != nil {
		t.Fatal(err)
	}
	godror.EnableDbmsOutput(ctx, conn)

	opts := []cmp.Option{
		cmp.Comparer(func(x, y time.Time) bool { return x.Equal(y) }),
	}

	for _, tC := range []struct {
		Name     string
		In, Want interface{}
	}{
		{Name: "vc", In: vc, Want: vcWant},
		{Name: "num", In: num, Want: numWant},
		{Name: "dt", In: dt, Want: dtWant},
		// {Name: "int", In: intgr, Want: intgrWant},
		{Name: "vc-1", In: vc[:1], Want: []string{"string +", "1"}},
		{Name: "vc-0", In: vc[:0], Want: []string{"0"}},
	} {
		tC := tC
		t.Run("inout_"+tC.Name, func(t *testing.T) {
			t.Logf("%s=%s", tC.Name, tC.In)
			nm := strings.SplitN(tC.Name, "-", 2)[0]
			qry = "BEGIN " + pkg + ".inout_" + nm + "(:1); END;"
			dst := copySlice(tC.In)
			if _, err := conn.ExecContext(ctx, qry,
				godror.PlSQLArrays,
				sql.Out{Dest: dst, In: true},
			); err != nil {
				t.Fatalf("%s\n%#v\n%+v", qry, dst, err)
			}
			got := reflect.ValueOf(dst).Elem().Interface()
			if nm == "dt" {
				t.Logf("\nin =%v\ngot=%v\nwt= %v", tC.In, got, tC.Want)
			}

			if cmp.Equal(got, tC.Want, opts...) {
				return
			}
			t.Errorf("%s: %s", tC.Name, cmp.Diff(printSlice(tC.Want), printSlice(got)))
			var buf bytes.Buffer
			if err := godror.ReadDbmsOutput(ctx, &buf, conn); err != nil {
				t.Error(err)
			}
			t.Log("OUTPUT:", buf.String())
		})
	}

	// lob := []godror.Lob{godror.Lob{IsClob: true, Reader: strings.NewReader("abcdef")}}
	t.Run("p2", func(t *testing.T) {
		if _, err := conn.ExecContext(ctx,
			"BEGIN "+pkg+".p2(:1, :2, :3); END;",
			godror.PlSQLArrays,
			// sql.Out{Dest: &intgr, In: true},
			sql.Out{Dest: &num, In: true},
			sql.Out{Dest: &vc, In: true},
			sql.Out{Dest: &dt, In: true},
			// sql.Out{Dest: &lob, In: true},
		); err != nil {
			t.Fatal(err)
		}
		t.Logf("int=%#v num=%#v vc=%#v dt=%#v", intgr, num, vc, dt)
		// if d := cmp.Diff(intgr, intgrWant); d != "" {
		//	t.Errorf("int: %s", d)
		// }
		if d := cmp.Diff(num, numWant); d != "" {
			t.Errorf("num: %s", d)
		}
		if d := cmp.Diff(vc, vcWant); d != "" {
			t.Errorf("vc: %s", d)
		}
		if !cmp.Equal(dt, dtWant, opts...) {
			if d := cmp.Diff(dt, dtWant); d != "" {
				t.Errorf("dt: %s", d)
			}
		}
	})
}

func TestOutParam(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("OutParam"), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ALTER SESSION SET time_zone = local"); err != nil {
		t.Fatal(err)
	}
	pkg := strings.ToUpper("test_p1" + tblSuffix)
	qry := `CREATE OR REPLACE PROCEDURE
` + pkg + `(p_int IN OUT INTEGER, p_num IN OUT NUMBER, p_vc IN OUT VARCHAR2, p_dt IN OUT DATE, p_lob IN OUT CLOB)
IS
BEGIN
  p_int := NVL(p_int * 2, 1);
  p_num := NVL(p_num / 2, 0.5);
  p_vc := NVL(p_vc ||' +', '-');
  p_dt := NVL(p_dt + 1, SYSDATE);
  p_lob := NULL;
END;`
	if _, err = conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	defer testDb.Exec("DROP PROCEDURE " + pkg)

	qry = "BEGIN " + pkg + "(:1, :2, :3, :4, :5); END;"
	stmt, err := conn.PrepareContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer stmt.Close()

	var intgr int = 3
	num := godror.Number("3.14")
	var vc string = "string"
	var dt time.Time = time.Date(2017, 6, 18, 7, 5, 51, 0, time.Local)
	var lob godror.Lob = godror.Lob{IsClob: true, Reader: strings.NewReader("abcdef")}
	if _, err := stmt.ExecContext(ctx,
		sql.Out{Dest: &intgr, In: true},
		sql.Out{Dest: &num, In: true},
		sql.Out{Dest: &vc, In: true},
		sql.Out{Dest: &dt, In: true},
		sql.Out{Dest: &lob, In: true},
	); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	t.Logf("int=%#v num=%#v vc=%#v dt=%#v", intgr, num, vc, dt)
	if intgr != 6 {
		t.Errorf("int: got %d, wanted %d", intgr, 6)
	}
	if num != "1.57" {
		t.Errorf("num: got %q, wanted %q", num, "1.57")
	}
	if vc != "string +" {
		t.Errorf("vc: got %q, wanted %q", vc, "string +")
	}
}

func TestSelectRefCursor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("SelectRefCursor"), 10*time.Second)
	defer cancel()
	rows, err := testDb.QueryContext(ctx, "SELECT CURSOR(SELECT object_name, object_type, object_id, created FROM all_objects WHERE ROWNUM <= 10) FROM DUAL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sub driver.Rows
		if err := rows.Scan(&sub); err != nil {
			sub.Close()
			t.Fatal(err)
		}
		defer sub.Close()
		t.Logf("%[1]T %[1]p", sub)
		cols := sub.(driver.RowsColumnTypeScanType).Columns()
		t.Log("Columns", cols)
		dests := make([]driver.Value, len(cols))
		for {
			if err := sub.Next(dests); err != nil {
				if err == io.EOF {
					break
				}
				sub.Close()
				t.Error(err)
				break
			}
			// fmt.Println(dests)
			t.Log(dests)
		}
		sub.Close()
	}
	// Test the Finalizers
	runtime.GC()
}

func TestSelectRefCursorWrap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("SelectRefCursorWrap"), 10*time.Second)
	defer cancel()
	rows, err := testDb.QueryContext(ctx, "SELECT CURSOR(SELECT object_name, object_type, object_id, created FROM all_objects WHERE ROWNUM <= 10) FROM DUAL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var intf driver.Rows
		if err := rows.Scan(&intf); err != nil {
			t.Error(err)
			continue
		}
		t.Logf("%[1]T %[1]p", intf)
		dr := intf.(driver.Rows)
		sub, err := godror.WrapRows(ctx, testDb, dr)
		if err != nil {
			dr.Close()
			t.Fatal(err)
		}
		t.Log("Sub", sub)
		for sub.Next() {
			var oName, oType, oID string
			var created time.Time
			if err := sub.Scan(&oName, &oType, &oID, &created); err != nil {
				dr.Close()
				sub.Close()
				t.Error(err)
				break
			}
			t.Log(oName, oType, oID, created)
		}
		dr.Close()
		sub.Close()
	}
	// Test the Finalizers
	runtime.GC()
}

func TestExecRefCursor(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("ExecRefCursor"), 30*time.Second)
	defer cancel()
	funName := "test_rc" + tblSuffix
	funQry := "CREATE OR REPLACE FUNCTION " + funName + ` RETURN SYS_REFCURSOR IS
  v_cur SYS_REFCURSOR;
BEGIN
  OPEN v_cur FOR SELECT object_name FROM all_objects WHERE ROWNUM < 10;
  RETURN(v_cur);
END;`
	if _, err := testDb.ExecContext(ctx, funQry); err != nil {
		t.Fatalf("%s: %v", funQry, err)
	}
	defer testDb.ExecContext(ctx, `DROP FUNCTION `+funName)
	qry := "BEGIN :1 := " + funName + "; END;"
	var dr driver.Rows
	if _, err := testDb.ExecContext(ctx, qry, sql.Out{Dest: &dr}); err != nil {
		t.Fatalf("%s: %v", qry, err)
	}
	defer dr.Close()
	sub, err := godror.WrapRows(ctx, testDb, dr)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	t.Log("Sub", sub)
	for sub.Next() {
		var s string
		if err := sub.Scan(&s); err != nil {
			t.Fatal(err)
		}
		t.Log(s)
	}
	runtime.GC()
	time.Sleep(time.Second)
	runtime.GC()
}

func TestExecuteMany(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()

	ctx, cancel := context.WithTimeout(testContext("ExecuteMany"), 30*time.Second)
	defer cancel()
	tbl := "test_em" + tblSuffix
	testDb.ExecContext(ctx, "DROP TABLE "+tbl)
	testDb.ExecContext(ctx, "CREATE TABLE "+tbl+" (f_id INTEGER, f_int INTEGER, f_num NUMBER, f_num_6 NUMBER(6), F_num_5_2 NUMBER(5,2), f_vc VARCHAR2(30), F_dt DATE)")
	defer testDb.Exec("DROP TABLE " + tbl)

	const num = 1000
	ints := make([]int, num)
	nums := make([]godror.Number, num)
	int32s := make([]int32, num)
	floats := make([]float64, num)
	strs := make([]string, num)
	dates := make([]time.Time, num)

	tx, err := testDb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "ALTER SESSION SET time_zone = local"); err != nil {
		t.Fatal(err)
	}
	// This is instead of now: a nice moment in time right before the summer time shift
	now := time.Date(2017, 10, 29, 1, 27, 53, 0, time.Local).Truncate(time.Second)
	ids := make([]int, num)
	for i := range nums {
		ids[i] = i
		ints[i] = i << 1
		nums[i] = godror.Number(strconv.Itoa(i))
		int32s[i] = int32(i)
		floats[i] = float64(i) / float64(3.14)
		strs[i] = fmt.Sprintf("%x", i)
		dates[i] = now.Add(-time.Duration(i) * time.Hour)
	}
	for i, tc := range []struct {
		Name  string
		Value interface{}
	}{
		{"f_int", ints},
		{"f_num", nums},
		{"f_num_6", int32s},
		{"f_num_5_2", floats},
		{"f_vc", strs},
		{"f_dt", dates},
	} {
		res, execErr := tx.ExecContext(ctx,
			"INSERT INTO "+tbl+" ("+tc.Name+") VALUES (:1)", //nolint:gas
			tc.Value)
		if execErr != nil {
			t.Fatalf("%d. INSERT INTO "+tbl+" (%q) VALUES (%+v): %#v", //nolint:gas
				i, tc.Name, tc.Value, execErr)
		}
		ra, raErr := res.RowsAffected()
		if raErr != nil {
			t.Error(raErr)
		} else if ra != num {
			t.Errorf("%d. %q: wanted %d rows, got %d", i, tc.Name, num, ra)
		}
	}
	tx.Rollback()

	testDb.ExecContext(ctx, "TRUNCATE TABLE "+tbl+"")

	if tx, err = testDb.BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO `+tbl+ //nolint:gas
			` (f_id, f_int, f_num, f_num_6, F_num_5_2, F_vc, F_dt)
			VALUES
			(:1, :2, :3, :4, :5, :6, :7)`,
		ids, ints, nums, int32s, floats, strs, dates)
	if err != nil {
		t.Fatalf("%#v", err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		t.Error(err)
	} else if ra != num {
		t.Errorf("wanted %d rows, got %d", num, ra)
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT * FROM "+tbl+" ORDER BY F_id", //nolint:gas
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id, Int int
		var num string
		var vc string
		var num6 int32
		var num52 float64
		var dt time.Time
		if err := rows.Scan(&id, &Int, &num, &num6, &num52, &vc, &dt); err != nil {
			t.Fatal(err)
		}
		if id != i {
			t.Fatalf("ID got %d, wanted %d.", id, i)
		}
		if Int != ints[i] {
			t.Errorf("%d. INT got %d, wanted %d.", i, Int, ints[i])
		}
		if num != string(nums[i]) {
			t.Errorf("%d. NUM got %q, wanted %q.", i, num, nums[i])
		}
		if num6 != int32s[i] {
			t.Errorf("%d. NUM_6 got %v, wanted %v.", i, num6, int32s[i])
		}
		rounded := float64(int64(floats[i]/0.005+0.5)) * 0.005
		if math.Abs(num52-rounded) > 0.05 {
			t.Errorf("%d. NUM_5_2 got %v, wanted %v.", i, num52, rounded)
		}
		if vc != strs[i] {
			t.Errorf("%d. VC got %q, wanted %q.", i, vc, strs[i])
		}
		t.Logf("%d. dt=%v", i, dt)
		if !dt.Equal(dates[i]) {
			if fmt.Sprintf("%v", dt) == "2017-10-29 02:27:53 +0100 CET" &&
				fmt.Sprintf("%v", dates[i]) == "2017-10-29 00:27:53 +0000 UTC" {
				t.Logf("%d. got DT %v, wanted %v (%v)", i, dt, dates[i], dt.Sub(dates[i]))
			} else {
				t.Errorf("%d. got DT %v, wanted %v (%v)", i, dt, dates[i], dt.Sub(dates[i]))
			}
		}
		i++
	}
}
func TestReadWriteLob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("ReadWriteLob"), 30*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tbl := "test_lob" + tblSuffix
	conn.ExecContext(ctx, "DROP TABLE "+tbl)
	conn.ExecContext(ctx,
		"CREATE TABLE "+tbl+" (f_id NUMBER(6), f_blob BLOB, f_clob CLOB)", //nolint:gas
	)
	defer testDb.Exec(
		"DROP TABLE " + tbl, //nolint:gas
	)

	stmt, err := conn.PrepareContext(ctx,
		"INSERT INTO "+tbl+" (F_id, f_blob, F_clob) VALUES (:1, :2, :3)", //nolint:gas
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	for tN, tC := range []struct {
		Bytes  []byte
		String string
	}{
		{[]byte{0, 1, 2, 3, 4, 5}, "12345"},
	} {

		if _, err = stmt.ExecContext(ctx, tN*2, tC.Bytes, tC.String); err != nil {
			t.Errorf("%d/1. (%v, %q): %v", tN, tC.Bytes, tC.String, err)
			continue
		}
		if _, err = stmt.ExecContext(ctx, tN*3+1,
			godror.Lob{Reader: bytes.NewReader(tC.Bytes)},
			godror.Lob{Reader: strings.NewReader(tC.String), IsClob: true},
		); err != nil {
			t.Errorf("%d/2. (%v, %q): %v", tN, tC.Bytes, tC.String, err)
		}

		var rows *sql.Rows
		rows, err = conn.QueryContext(ctx,
			"SELECT F_id, F_blob, F_clob FROM "+tbl+" WHERE F_id IN (:1, :2)", //nolint:gas
			godror.LobAsReader(),
			2*tN, 2*tN+1)
		if err != nil {
			t.Errorf("%d/3. %v", tN, err)
			continue
		}
		for rows.Next() {
			var id, blob, clob interface{}
			if err = rows.Scan(&id, &blob, &clob); err != nil {
				rows.Close()
				t.Errorf("%d/3. scan: %v", tN, err)
				continue
			}
			t.Logf("%d. blob=%+v clob=%+v", id, blob, clob)
			if blob, ok := blob.(*godror.Lob); !ok {
				t.Errorf("%d. %T is not LOB", id, blob)
			} else {
				got := make([]byte, len(tC.Bytes))
				n, err := io.ReadFull(blob, got)
				t.Logf("%d. BLOB read %d (%q =%t= %q): %+v", id, n, got, bytes.Equal(got, tC.Bytes), tC.Bytes, err)
				if err != nil {
					t.Errorf("%d. %v", id, err)
				} else if !bytes.Equal(got, tC.Bytes) {
					t.Errorf("%d. got %q for BLOB, wanted %q", id, got, tC.Bytes)
				}
			}
			if clob, ok := clob.(*godror.Lob); !ok {
				t.Errorf("%d. %T is not LOB", id, clob)
			} else {
				var got []byte
				if got, err = ioutil.ReadAll(clob); err != nil {
					t.Errorf("%d. %v", id, err)
				} else if got := string(got); got != tC.String {
					t.Errorf("%d. got %q for CLOB, wanted %q", id, got, tC.String)
				}
			}
		}
		rows.Close()
	}

	rows, err := conn.QueryContext(ctx,
		"SELECT F_blob, F_clob FROM "+tbl+"", //nolint:gas
		godror.ClobAsString())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var s string
		if err = rows.Scan(&b, &s); err != nil {
			t.Error(err)
		}
		t.Logf("clobAsString: %q", s)
	}

	qry := "SELECT CURSOR(SELECT f_id, F_blob, f_clob FROM " + tbl + " WHERE ROWNUM <= 10) FROM DUAL"
	rows, err = testDb.QueryContext(ctx, qry, godror.ClobAsString())
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()
	for rows.Next() {
		var intf interface{}
		if err := rows.Scan(&intf); err != nil {
			t.Error(err)
			continue
		}
		t.Logf("%T", intf)
		sub := intf.(driver.RowsColumnTypeScanType)
		cols := sub.Columns()
		t.Log("Columns", cols)
		dests := make([]driver.Value, len(cols))
		for {
			if err := sub.Next(dests); err != nil {
				if err == io.EOF {
					break
				}
				t.Error(err)
				break
			}
			// fmt.Println(dests)
			t.Log(dests)
		}
		sub.Close()
	}

}

func TestReadWriteBfile(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("ReadWritBfile"), 30*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tbl := "test_Bfile" + tblSuffix
	conn.ExecContext(ctx, "DROP TABLE "+tbl)
	conn.ExecContext(ctx,
		"CREATE TABLE "+tbl+" (f_id NUMBER(6), f_bf BFILE)", //nolint:gas
	)
	defer testDb.Exec(
		"DROP TABLE " + tbl, //nolint:gas
	)

	stmt, err := conn.PrepareContext(ctx,
		"INSERT INTO "+tbl+" (F_id, f_bf) VALUES (:1, BFILENAME(:2, :3))", //nolint:gas
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	for tN, tC := range []struct {
		Dir  string
		File string
	}{
		{"TEST", "1.txt"},
	} {

		if _, err = stmt.ExecContext(ctx, tN*2, tC.Dir, tC.File); err != nil {
			t.Errorf("%d/1. (%s, %s): %v", tN, tC.Dir, tC.File, err)
			continue
		}

		var rows *sql.Rows
		rows, err = conn.QueryContext(ctx,
			"SELECT F_id, F_bf FROM "+tbl+" WHERE F_id = :1", //nolint:gas
			2*tN)
		if err != nil {
			t.Errorf("%d/2. %v", tN, err)
			continue
		}
		for rows.Next() {
			var id, bfile interface{}
			if err = rows.Scan(&id, &bfile); err != nil {
				rows.Close()
				t.Errorf("%d/2. scan: %v", tN, err)
				continue
			}
			t.Logf("%d. bfile=%+v", id, bfile)
			if b, ok := bfile.(*godror.Lob); !ok {
				t.Errorf("%d. %T is not LOB", id, b)
			} else {
				lobD, err := b.Hijack()
				if err != nil {
					t.Error(err)
				}
				dir, file, err := lobD.GetFileName()
				if err != nil {
					t.Error(err)
				}
				if dir != tC.Dir {
					t.Errorf("the got dir %v not equal want %v", dir, tC.Dir)
				}
				if file != tC.File {
					t.Errorf("the got file %v not equal want %v", file, tC.File)
				}
			}
		}
		rows.Close()
	}
}

func printSlice(orig interface{}) interface{} {
	ro := reflect.ValueOf(orig)
	if ro.Kind() == reflect.Ptr {
		ro = ro.Elem()
	}
	ret := make([]string, 0, ro.Len())
	for i := 0; i < ro.Len(); i++ {
		ret = append(ret, fmt.Sprintf("%v", ro.Index(i).Interface()))
	}
	return ret
}
func copySlice(orig interface{}) interface{} {
	ro := reflect.ValueOf(orig)
	rc := reflect.New(reflect.TypeOf(orig)).Elem() // *[]s
	rc.Set(reflect.MakeSlice(ro.Type(), ro.Len(), ro.Cap()+1))
	for i := 0; i < ro.Len(); i++ {
		rc.Index(i).Set(ro.Index(i))
	}
	return rc.Addr().Interface()
}

func TestOpenClose(t *testing.T) {
	cs, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	cs.MinSessions, cs.MaxSessions = 1, 6
	t.Log(cs.String())
	db, err := sql.Open("godror", cs.StringWithPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cErr := db.Close(); cErr != nil {
			t.Error("CLOSE:", cErr)
		}
	}()
	db.SetMaxIdleConns(cs.MinSessions)
	db.SetMaxOpenConns(cs.MaxSessions)
	ctx, cancel := context.WithCancel(testContext("OpenClose"))
	defer cancel()
	const module = "godror.v2.test-OpenClose "
	const countQry = "SELECT COUNT(0) FROM v$session WHERE module LIKE '" + module + "%'"
	stmt, err := db.PrepareContext(ctx, countQry)
	if err != nil {
		if strings.Contains(err.Error(), "ORA-12516:") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	defer stmt.Close()

	sessCount := func() (n int, stats godror.PoolStats, err error) {
		var sErr error
		sErr = godror.Raw(ctx, db, func(cx godror.Conn) error {
			var gErr error
			stats, gErr = cx.GetPoolStats()
			return gErr
		})
		if qErr := stmt.QueryRowContext(ctx).Scan(&n); qErr != nil && sErr == nil {
			sErr = qErr
		}
		return n, stats, sErr
	}
	n, ps, err := sessCount()
	if err != nil {
		t.Skip(err)
	}
	if n > 0 {
		t.Logf("sessCount=%d, stats=%s at start!", n, ps)
	}
	var tt godror.TraceTag
	for i := 0; i < cs.MaxSessions*2; i++ {
		t.Logf("%d. PREPARE", i+1)
		stmt, err := db.PrepareContext(ctx, "SELECT 1 FROM DUAL")
		if err != nil {
			t.Fatal(err)
		}
		if n, ps, err = sessCount(); err != nil {
			t.Error(err)
		} else {
			t.Logf("sessCount=%d stats=%s", n, ps)
		}
		tt.Module = fmt.Sprintf("%s%d", module, 2*i)
		ctx = godror.ContextWithTraceTag(ctx, tt)
		tx1, err1 := db.BeginTx(ctx, nil)
		if err1 != nil {
			t.Fatal(err1)
		}
		tt.Module = fmt.Sprintf("%s%d", module, 2*i+1)
		ctx = godror.ContextWithTraceTag(ctx, tt)
		tx2, err2 := db.BeginTx(ctx, nil)
		if err2 != nil {
			if strings.Contains(err2.Error(), "ORA-12516:") {
				tx1.Rollback()
				break
			}
			t.Fatal(err2)
		}
		if n, ps, err = sessCount(); err != nil {
			t.Log(err)
		} else if n == 0 {
			t.Errorf("sessCount=0, stats=%s want at least 2", ps)
		} else {
			t.Logf("sessCount=%d stats=%s", n, ps)
		}
		tx1.Rollback()
		tx2.Rollback()
		stmt.Close()
	}
	if n, ps, err = sessCount(); err != nil {
		t.Log(err)
	} else if n > 7 {
		t.Errorf("sessCount=%d stats=%s", n, ps)
	}
}

func TestOpenBadMemory(t *testing.T) {
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	t.Log("Allocated 0:", mem.Alloc)
	zero := mem.Alloc
	for i := 0; i < 100; i++ {
		badConStr := strings.Replace(testConStr, "@", fmt.Sprintf("BAD%dBAD@", i), 1)
		db, err := sql.Open("godror", badConStr)
		if err != nil {
			t.Fatalf("bad connection string %q didn't produce error!", badConStr)
		}
		db.Close()
		runtime.GC()
		runtime.ReadMemStats(&mem)
		t.Logf("Allocated %d: %d", i+1, mem.Alloc)
	}
	d := mem.Alloc - zero
	if mem.Alloc < zero {
		d = 0
	}
	t.Logf("atlast: %d", d)
	if d > 64<<10 {
		t.Errorf("Consumed more than 64KiB of memory: %d", d)
	}
}

func TestSelectFloat(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("SelectFloat"), 10*time.Second)
	defer cancel()
	tbl := "test_numbers" + tblSuffix
	qry := `CREATE TABLE ` + tbl + ` (
  INT_COL     NUMBER,
  FLOAT_COL  NUMBER,
  EMPTY_INT_COL NUMBER
)`
	testDb.Exec("DROP TABLE " + tbl)
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer testDb.Exec("DROP TABLE " + tbl)

	const INT, FLOAT = 1234567, 4.5
	qry = `INSERT INTO ` + tbl + //nolint:gas
		` (INT_COL, FLOAT_COL, EMPTY_INT_COL)
     VALUES (1234567, 45/10, NULL)`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}

	qry = "SELECT int_col, float_col, empty_int_col FROM " + tbl //nolint:gas
	type numbers struct {
		Int     int
		Int64   int64
		Float   float64
		NInt    sql.NullInt64
		String  string
		NString sql.NullString
		Number  godror.Number
	}
	var n numbers
	var i1, i2, i3 interface{}
	for tName, tC := range map[string]struct {
		Dest [3]interface{}
		Want numbers
	}{
		"int,float,nstring": {
			Dest: [3]interface{}{&n.Int, &n.Float, &n.NString},
			Want: numbers{Int: INT, Float: FLOAT},
		},
		"inf,float,Number": {
			Dest: [3]interface{}{&n.Int, &n.Float, &n.Number},
			Want: numbers{Int: INT, Float: FLOAT},
		},
		"int64,float,nullInt": {
			Dest: [3]interface{}{&n.Int64, &n.Float, &n.NInt},
			Want: numbers{Int64: INT, Float: FLOAT},
		},
		"intf,intf,intf": {
			Dest: [3]interface{}{&i1, &i2, &i3},
			Want: numbers{Int64: INT, Float: FLOAT},
		},
	} {
		i1, i2, i3 = nil, nil, nil
		n = numbers{}
		F := func() error {
			err := testDb.QueryRowContext(ctx, qry).Scan(tC.Dest[0], tC.Dest[1], tC.Dest[2])
			if err != nil {
				err = fmt.Errorf("%s: %w", qry, err)
			}
			return err
		}
		if err := F(); err != nil {
			if strings.HasSuffix(err.Error(), "unsupported Scan, storing driver.Value type <nil> into type *string") {
				t.Log("WARNING:", err)
				continue
			}
			noLogging := tl.enableLogging(t)
			err = F()
			t.Errorf("%q: %v", tName, fmt.Errorf("%s: %w", qry, err))
			noLogging()
			continue
		}
		if tName == "intf,intf,intf" {
			t.Logf("%q: %#v, %#v, %#v", tName, i1, i2, i3)
			continue
		}
		t.Logf("%q: %+v", tName, n)
		if n != tC.Want {
			t.Errorf("%q:\ngot\t%+v,\nwanted\t%+v.", tName, n, tC.Want)
		}
	}
}

func TestNumInputs(t *testing.T) {
	t.Parallel()
	var a, b string
	if err := testDb.QueryRow("SELECT :1, :2 FROM DUAL", 'a', 'b').Scan(&a, &b); err != nil {
		t.Errorf("two inputs: %+v", err)
	}
	if err := testDb.QueryRow("SELECT :a, :b FROM DUAL", 'a', 'b').Scan(&a, &b); err != nil {
		t.Errorf("two named inputs: %+v", err)
	}
	if err := testDb.QueryRow("SELECT :a, :a FROM DUAL", sql.Named("a", a)).Scan(&a, &b); err != nil {
		t.Errorf("named inputs: %+v", err)
	}
}

func TestPtrArg(t *testing.T) {
	t.Parallel()
	s := "dog"
	rows, err := testDb.Query("SELECT * FROM user_objects WHERE object_name=:1", &s)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
}

func TestRanaOraIssue244(t *testing.T) {
	tableName := "test_ora_issue_244" + tblSuffix
	qry := "CREATE TABLE " + tableName + " (FUND_ACCOUNT VARCHAR2(18) NOT NULL, FUND_CODE VARCHAR2(6) NOT NULL, BUSINESS_FLAG NUMBER(10) NOT NULL, MONEY_TYPE VARCHAR2(3) NOT NULL)"
	testDb.Exec("DROP TABLE " + tableName)
	if _, err := testDb.Exec(qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	var max int
	ctx, cancel := context.WithCancel(testContext("RanaOraIssue244-1"))
	txs := make([]*sql.Tx, 0, maxSessions/2)
	for max = 0; max < maxSessions/2; max++ {
		tx, err := testDb.BeginTx(ctx, nil)
		if err != nil {
			max--
			break
		}
		txs = append(txs, tx)
	}
	for _, tx := range txs {
		tx.Rollback()
	}
	cancel()
	t.Logf("maxSessions=%d max=%d", maxSessions, max)

	dur := time.Minute / 2
	if testing.Short() {
		dur = 10 * time.Second
	}
	ctx, cancel = context.WithTimeout(testContext("RanaOraIssue244-2"), dur)
	defer cancel()
	defer testDb.Exec("DROP TABLE " + tableName)
	const bf = "143"
	const sc = "270004"
	qry = "INSERT INTO " + tableName + " (fund_account, fund_code, business_flag, money_type) VALUES (:1, :2, :3, :4)" //nolint:gas
	tx, err := testDb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	fas := []string{"14900666", "1868091", "1898964", "14900397"}
	for _, v := range fas {
		if _, err := stmt.ExecContext(ctx, v, sc, bf, "0"); err != nil {
			stmt.Close()
			t.Fatal(err)
		}
	}
	stmt.Close()
	tx.Commit()

	qry = `SELECT fund_account, money_type FROM ` + tableName + ` WHERE business_flag = :1 AND fund_code = :2 AND fund_account = :3` //nolint:gas
	grp, grpCtx := errgroup.WithContext(ctx)
	for i := 0; i < max; i++ {
		index := rand.Intn(len(fas))
		i, qry := i, qry
		grp.Go(func() error {
			tx, err := testDb.BeginTx(grpCtx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			defer tx.Rollback()

			stmt, err := tx.PrepareContext(grpCtx, qry)
			if err != nil {
				return fmt.Errorf("%d.Prepare %q: %w", i, qry, err)
			}
			defer stmt.Close()

			for j := 0; j < 3; j++ {
				select {
				case <-grpCtx.Done():
					return grpCtx.Err()
				default:
				}
				index = (index + 1) % len(fas)
				rows, err := stmt.QueryContext(grpCtx, bf, sc, fas[index])
				if err != nil {
					return fmt.Errorf("%d.tx=%p stmt=%p %d. %q: %w", i, tx, stmt, j, qry, err)
				}

				for rows.Next() {
					var acc, mt string
					if err = rows.Scan(&acc, &mt); err != nil {
						err = fmt.Errorf("Scan: %w", err)
						break
					}

					if acc != fas[index] {
						err = fmt.Errorf("got acc %q, wanted %q", acc, fas[index])
						break
					}
					if mt != "0" {
						err = fmt.Errorf("got mt %q, wanted 0", mt)
						break
					}
				}
				rows.Close()
				if err == nil {
					err = rows.Err()
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := grp.Wait(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, driver.ErrBadConn) {
			return
		}
		errS := errors.Unwrap(err).Error()
		switch errS {
		case "sql: statement is closed",
			"sql: transaction has already been committed or rolled back":
			return
		}
		if strings.Contains(errS, "ORA-12516:") || strings.Contains(errS, "ORA-24496:") {
			t.Log(err)
		} else {
			t.Error(err)
		}
	}
}

func TestNumberMarshal(t *testing.T) {
	t.Parallel()
	var n godror.Number
	if err := testDb.QueryRow("SELECT 6000370006565900000073 FROM DUAL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Log(n.String())
	b, err := n.MarshalJSON()
	t.Logf("%s", b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte{'e'}) {
		t.Errorf("got %q, wanted without scientific notation", b)
	}
	if b, err = json.Marshal(struct {
		N godror.Number
	}{N: n},
	); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", b)
}

func TestExecHang(t *testing.T) {
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("ExecHang"), 1*time.Second)
	defer cancel()
	done := make(chan error, 13)
	var wg sync.WaitGroup
	for i := 0; i < cap(done); i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				done <- err
				return
			}
			_, err := testDb.ExecContext(ctx, "DECLARE v_deadline DATE := SYSDATE + 3/24/3600; v_db PLS_INTEGER; BEGIN LOOP SELECT COUNT(0) INTO v_db FROM cat; EXIT WHEN SYSDATE >= v_deadline; END LOOP; END;")
			if err == nil {
				done <- fmt.Errorf("%d. wanted timeout got %v", i, err)
			}
			t.Logf("%d. %v", i, err)
		}()
	}
	wg.Wait()
	close(done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

}

func TestNumberNull(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("NumberNull"), time.Minute)
	defer cancel()
	testDb.Exec("DROP TABLE number_test")
	qry := `CREATE TABLE number_test (
		caseNum NUMBER(3),
		precisionNum NUMBER(5),
      precScaleNum NUMBER(5, 0),
		normalNum NUMBER
		)`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer testDb.Exec("DROP TABLE number_test")

	qry = `
		INSERT ALL
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (1, 4, 65, 123)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (2, NULL, NULL, NULL)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (3, NULL, NULL, NULL)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (4, NULL, 42, NULL)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (5, NULL, NULL, 31)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (6, 3, 3, 4)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (7, NULL, NULL, NULL)
		INTO number_test (caseNum, precisionNum, precScaleNum, normalNum) VALUES (8, 6, 9, 7)
		SELECT 1 FROM DUAL`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	qry = "SELECT precisionNum, precScaleNum, normalNum FROM number_test ORDER BY caseNum"
	rows, err := testDb.Query(qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()

	for rows.Next() {
		var precisionNum, recScaleNum, normalNum sql.NullInt64
		if err = rows.Scan(&precisionNum, &recScaleNum, &normalNum); err != nil {
			t.Fatal(err)
		}

		t.Log(precisionNum, recScaleNum, normalNum)

		if precisionNum.Int64 == 0 && precisionNum.Valid {
			t.Errorf("precisionNum=%v, wanted {0 false}", precisionNum)
		}
		if recScaleNum.Int64 == 0 && recScaleNum.Valid {
			t.Errorf("recScaleNum=%v, wanted {0 false}", recScaleNum)
		}
	}

	rows, err = testDb.Query(qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()

	for rows.Next() {
		var precisionNumStr, recScaleNumStr, normalNumStr sql.NullString
		if err = rows.Scan(&precisionNumStr, &recScaleNumStr, &normalNumStr); err != nil {
			t.Fatal(err)
		}
		t.Log(precisionNumStr, recScaleNumStr, normalNumStr)
	}
}

func TestNullFloat(t *testing.T) {
	t.Parallel()
	testDb.Exec("DROP TABLE test_char")
	if _, err := testDb.Exec(`CREATE TABLE test_char (
			CHARS VARCHAR2(10 BYTE),
			FLOATS NUMBER(10, 2)
		)`); err != nil {
		t.Fatal(err)
	}
	defer testDb.Exec("DROP TABLE test_char")

	tx, err := testDb.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		"INSERT INTO test_char VALUES(:CHARS, :FLOATS)",
		[]string{"dog", "", "cat"},
		/*[]sql.NullString{sql.NullString{"dog", true},
		sql.NullString{"", false},
		sql.NullString{"cat", true}},*/
		[]sql.NullFloat64{
			{Float64: 3.14, Valid: true},
			{Float64: 12.36, Valid: true},
			{Float64: 0.0, Valid: false},
		},
	)
	if err != nil {
		t.Error(err)
	}
}

func TestColumnSize(t *testing.T) {
	t.Parallel()
	testDb.Exec("DROP TABLE test_column_size")
	if _, err := testDb.Exec(`CREATE TABLE test_column_size (
		vc20b VARCHAR2(20 BYTE),
		vc1b VARCHAR2(1 BYTE),
		nvc20 NVARCHAR2(20),
		nvc1 NVARCHAR2(1),
		vc20c VARCHAR2(20 CHAR),
		vc1c VARCHAR2(1 CHAR)
	)`); err != nil {
		t.Fatal(err)
	}
	defer testDb.Exec("DROP TABLE test_column_size")

	r, err := testDb.Query("SELECT * FROM test_column_size")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rts, err := r.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range rts {
		l, _ := col.Length()

		t.Logf("Column %q has length %v", col.Name(), l)
	}
}

func TestReturning(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	testDb.Exec("DROP TABLE test_returning")
	if _, err := testDb.Exec("CREATE TABLE test_returning (a VARCHAR2(20))"); err != nil {
		t.Fatal(err)
	}
	defer testDb.Exec("DROP TABLE test_returning")

	want := "abraca dabra"
	var got string
	if _, err := testDb.Exec(
		`INSERT INTO test_returning (a) VALUES (UPPER(:1)) RETURNING a INTO :2`,
		want, sql.Out{Dest: &got},
	); err != nil {
		t.Fatal(err)
	}
	want = strings.ToUpper(want)
	if want != got {
		t.Errorf("got %q, wanted %q", got, want)
	}

	if _, err := testDb.Exec(
		`UPDATE test_returning SET a = '1' WHERE 1=0 RETURNING a /*LASTINSERTID*/ INTO :1`,
		sql.Out{Dest: &got},
	); err != nil {
		t.Fatal(err)
	}
	t.Logf("RETURNING (zero set): %v", got)
}

func TestMaxOpenCursorsORA1000(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(testContext("ORA1000"))
	defer cancel()
	rows, err := testDb.QueryContext(ctx, "SELECT * FROM user_objects WHERE ROWNUM < 100")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var openCursors sql.NullInt64
	const qry1 = "SELECT p.value FROM v$parameter p WHERE p.name = 'open_cursors'"
	if err := testDb.QueryRowContext(ctx, qry1).Scan(&openCursors); err == nil {
		t.Logf("open_cursors=%v", openCursors)
	} else {
		if err := testDb.QueryRow(qry1).Scan(&openCursors); err != nil {
			var cErr interface{ Code() int }
			if errors.As(err, &cErr) && cErr.Code() == 942 {
				t.Logf("%s: %+v", qry1, err)
			} else {
				t.Error(fmt.Errorf("%s: %w", qry1, err))
			}
		} else {
			t.Log(fmt.Errorf("%s: %w", qry1, err))
		}
	}
	n := int(openCursors.Int64)
	if 0 <= n || n >= 100 {
		n = 100
	}
	n *= 2
	for i := 0; i < n; i++ {
		var cnt int64
		qry2 := "SELECT /* " + strconv.Itoa(i) + " */ 1 FROM DUAL"
		if err = testDb.QueryRowContext(ctx, qry2).Scan(&cnt); err != nil {
			t.Fatal(fmt.Errorf("%d. %s: %w", i, qry2, err))
		}
	}
}

func TestRO(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(testContext("RO"))
	defer cancel()
	tx, err := testDb.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.QueryContext(ctx, "SELECT 1 FROM DUAL"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE test_table (i INTEGER)"); err == nil {
		t.Log("RO allows CREATE TABLE ?")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNullIntoNum(t *testing.T) {
	t.Parallel()
	testDb.Exec("DROP TABLE test_null_num")
	qry := "CREATE TABLE test_null_num (i NUMBER(3))"
	if _, err := testDb.Exec(qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer testDb.Exec("DROP TABLE test_null_num")

	qry = "INSERT INTO test_null_num (i) VALUES (:1)"
	var i *int
	if _, err := testDb.Exec(qry, i); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	P.Username += "--BAD---"
	badDB, err := sql.Open("godror", P.String())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(testContext("Ping"), 1*time.Second)
	defer cancel()

	dl, _ := ctx.Deadline()
	err = badDB.PingContext(ctx)
	ok := dl.After(time.Now())
	if err != nil {
		t.Log(err)
	} else {
		t.Log("ping succeeded")
		if !ok {
			t.Error("ping succeeded after deadline!")
		}
	}
}

func TestNoConnectionPooling(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("godror",
		strings.Replace(
			strings.Replace(testConStr, "TestClassName", godror.NoConnectionPoolingConnectionClass, 1),
			"standaloneConnection=0", "standaloneConnection=1", 1,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

func TestExecTimeout(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("ExecTimeout"), 100*time.Millisecond)
	defer cancel()
	if _, err := testDb.ExecContext(ctx, "DECLARE cnt PLS_INTEGER; BEGIN SELECT COUNT(0) INTO cnt FROM (SELECT 1 FROM all_objects WHERE ROWNUM < 1000), (SELECT 1 FROM all_objects WHERE rownum < 1000); END;"); err != nil {
		t.Log(err)
	}
}

func TestQueryTimeout(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(testContext("QueryTimeout"), 100*time.Millisecond)
	defer cancel()
	if _, err := testDb.QueryContext(ctx, "SELECT COUNT(0) FROM (SELECT 1 FROM all_objects WHERE rownum < 1000), (SELECT 1 FROM all_objects WHERE rownum < 1000)"); err != nil {
		t.Log(err)
	}
}

func TestSDO(t *testing.T) {
	// t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("SDO"), 30*time.Second)
	defer cancel()
	innerQry := `SELECT MDSYS.SDO_GEOMETRY(
	3001,
	NULL,
	NULL,
	MDSYS.SDO_ELEM_INFO_ARRAY(
		1,1,1,4,1,0,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL
	),
	MDSYS.SDO_ORDINATE_ARRAY(
		480736.567,10853969.692,0,0.998807402795312,-0.0488238888381834,0,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)
		) SHAPE FROM DUAL`
	selectQry := `SELECT shape, DUMP(shape), CASE WHEN shape IS NULL THEN 'I' ELSE 'N' END FROM (` + innerQry + ")"
	rows, err := testDb.QueryContext(ctx, selectQry)
	if err != nil {
		if !strings.Contains(err.Error(), `ORA-00904: "MDSYS"."SDO_GEOMETRY"`) {
			t.Fatal(fmt.Errorf("%s: %w", selectQry, err))
		}
		for _, qry := range []string{
			`CREATE TYPE test_sdo_point_type AS OBJECT (
			   X NUMBER,
			   Y NUMBER,
			   Z NUMBER)`,
			"CREATE TYPE test_sdo_elem_info_array AS VARRAY (1048576) of NUMBER",
			"CREATE TYPE test_sdo_ordinate_array AS VARRAY (1048576) of NUMBER",
			`CREATE TYPE test_sdo_geometry AS OBJECT (
			 SDO_GTYPE NUMBER,
			 SDO_SRID NUMBER,
			 SDO_POINT test_SDO_POINT_TYPE,
			 SDO_ELEM_INFO test_SDO_ELEM_INFO_ARRAY,
			 SDO_ORDINATES test_SDO_ORDINATE_ARRAY)`,

			`CREATE TABLE test_sdo(
					id INTEGER not null,
					shape test_SDO_GEOMETRY not null
				)`,
		} {
			var drop string
			if strings.HasPrefix(qry, "CREATE TYPE") {
				drop = "DROP TYPE " + qry[12:strings.Index(qry, " AS")] + " FORCE"
			} else {
				drop = "DROP TABLE " + qry[13:strings.Index(qry, "(")]
			}
			testDb.ExecContext(ctx, drop)
			t.Log(drop)
			if _, err = testDb.ExecContext(ctx, qry); err != nil {
				err = fmt.Errorf("%s: %w", qry, err)
				t.Log(err)
				if !strings.Contains(err.Error(), "ORA-01031:") {
					t.Fatal(err)
				}
				t.Skip(err)
			}
			defer testDb.ExecContext(ctx, drop)
		}

		selectQry = strings.Replace(selectQry, "MDSYS.SDO_", "test_SDO_", -1)
		if rows, err = testDb.QueryContext(ctx, selectQry); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", selectQry, err))
		}

	}
	defer rows.Close()
	if false {
		godror.Log = func(kv ...interface{}) error {
			t.Helper()
			t.Log(kv)
			return nil
		}
	}
	for rows.Next() {
		var dmp, isNull string
		var intf interface{}
		if err = rows.Scan(&intf, &dmp, &isNull); err != nil {
			t.Error(fmt.Errorf("%s: %w", "scan", err))
		}
		t.Log(dmp, isNull)
		obj := intf.(*godror.Object)
		// t.Log("obj:", obj)
		printObj(t, "", obj)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func printObj(t *testing.T, name string, obj *godror.Object) {
	if obj == nil {
		return
	}
	for key := range obj.Attributes {
		sub, err := obj.Get(key)
		t.Logf("%s.%s. %+v (err=%+v)\n", name, key, sub, err)
		if err != nil {
			t.Errorf("ERROR: %+v", err)
		}
		if ss, ok := sub.(*godror.Object); ok {
			printObj(t, name+"."+key, ss)
		} else if coll, ok := sub.(*godror.ObjectCollection); ok {
			slice, err := coll.AsSlice(nil)
			t.Logf("%s.%s. %+v", name, key, slice)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

var _ = driver.Valuer((*Custom)(nil))
var _ = sql.Scanner((*Custom)(nil))

type Custom struct {
	Num int64
}

func (t *Custom) Value() (driver.Value, error) {
	return t.Num, nil
}

func (t *Custom) Scan(v interface{}) error {
	var err error
	switch v := v.(type) {
	case int64:
		t.Num = v
	case string:
		t.Num, err = strconv.ParseInt(v, 10, 64)
	case float64:
		t.Num = int64(v)
	default:
		err = fmt.Errorf("unknown type %T", v)
	}
	return err
}

func TestSelectCustomType(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("SelectCustomType"), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tbl := "test_custom_type" + tblSuffix
	conn.ExecContext(ctx, "DROP TABLE "+tbl)
	qry := "CREATE TABLE " + tbl + " (nm VARCHAR2(30), typ VARCHAR2(30), id NUMBER(6), created DATE)"
	if _, err = conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer testDb.Exec("DROP TABLE " + tbl)

	n := 1000
	nms, typs, ids, createds := make([]string, n), make([]string, n), make([]int, n), make([]time.Time, n)
	now := time.Now()
	for i := range nms {
		nms[i], typs[i], ids[i], createds[i] = fmt.Sprintf("obj-%d", i), "OBJECT", i, now.Add(-time.Duration(i)*time.Second)
	}
	qry = "INSERT INTO " + tbl + " (nm, typ, id, created) VALUES (:1, :2, :3, :4)"
	if _, err = conn.ExecContext(ctx, qry, nms, typs, ids, createds); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}

	const num = 10
	nums := &Custom{Num: num}
	type underlying int64
	numbers := underlying(num)
	rows, err := conn.QueryContext(ctx,
		"SELECT nm, typ, id, created FROM "+tbl+" WHERE ROWNUM < COALESCE(:alpha, :beta, 2) ORDER BY id",
		sql.Named("alpha", nums),
		sql.Named("beta", int64(numbers)),
	)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	n = 0
	oldOid := int64(0)
	for rows.Next() {
		var tbl, typ string
		var oid int64
		var created time.Time
		if err := rows.Scan(&tbl, &typ, &oid, &created); err != nil {
			t.Fatal(err)
		}
		t.Log(tbl, typ, oid, created)
		if tbl == "" {
			t.Fatal("empty tbl")
		}
		n++
		if oldOid > oid {
			t.Errorf("got oid=%d, wanted sth < %d.", oid, oldOid)
		}
		oldOid = oid
	}
	if n == 0 || n > num {
		t.Errorf("got %d rows, wanted %d", n, num)
	}
}

func TestExecInt64(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("ExecInt64"), 10*time.Second)
	defer cancel()
	qry := `CREATE OR REPLACE PROCEDURE test_i64_out(p_int NUMBER, p_out1 OUT NUMBER, p_out2 OUT NUMBER) IS
	BEGIN p_out1 := p_int; p_out2 := p_int; END;`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err)
	}
	defer testDb.ExecContext(ctx, "DROP PROCEDURE test_i64_out")

	qry = "BEGIN test_i64_out(:1, :2, :3); END;"
	var num sql.NullInt64
	var str string
	defer tl.enableLogging(t)()
	if _, err := testDb.ExecContext(ctx, qry, 3.14, sql.Out{Dest: &num}, sql.Out{Dest: &str}); err != nil {
		t.Fatal(err)
	}
	t.Log("num:", num, "str:", str)
}

func TestImplicitResults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("ImplicitResults"), 10*time.Second)
	defer cancel()
	const qry = `declare
			c0 sys_refcursor;
            c1 sys_refcursor;
            c2 sys_refcursor;
        begin
			:1 := c0;
            open c1 for
            select 1 from DUAL;
            dbms_sql.return_result(c1);
            open c2 for
            select 'A' from DUAL;
            dbms_sql.return_result(c2);
        end;`
	var rows driver.Rows
	if _, err := testDb.ExecContext(ctx, qry, sql.Out{Dest: &rows}); err != nil {
		if strings.Contains(err.Error(), "PLS-00302:") {
			t.Skip()
		}
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()
	r := rows.(driver.RowsNextResultSet)
	for r.HasNextResultSet() {
		if err := r.NextResultSet(); err != nil {
			t.Error(err)
		}
	}
}

func TestStartupShutdown(t *testing.T) {
	if os.Getenv("GODROR_DB_SHUTDOWN") != "1" {
		t.Skip("GODROR_DB_SHUTDOWN != 1, skipping shutdown/startup test")
	}
	p, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", testConStr, err))
	}
	if !(p.IsSysDBA || p.IsSysOper) {
		p.IsSysDBA = true
	}
	if !p.IsPrelim {
		p.IsPrelim = true
	}
	db, err := sql.Open("godror", p.StringWithPassword())
	if err != nil {
		t.Fatal(err, p.StringWithPassword())
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(testContext("StartupShutdown"))
	defer cancel()

	if err = godror.Raw(ctx, db, func(conn godror.Conn) error {
		if err = conn.Shutdown(godror.ShutdownTransactionalLocal); err != nil {
			return err
		}
		if err = conn.Shutdown(godror.ShutdownFinal); err != nil {
			return err
		}
		return conn.Startup(godror.StartupDefault)
	}); err != nil {
		t.Error(err)
	}
}

func TestIssue134(t *testing.T) {
	cleanup := func() {
		for _, qry := range []string{
			`DROP TYPE test_prj_task_tab_type`,
			`DROP TYPE test_prj_task_obj_type`,
			`DROP PROCEDURE test_create_task_activity`,
		} {
			testDb.Exec(qry)
		}
	}
	cleanup()
	const crea = `CREATE OR REPLACE TYPE test_PRJ_TASK_OBJ_TYPE AS OBJECT (
	PROJECT_NUMBER VARCHAR2(100)
	,SOURCE_ID VARCHAR2(100)
	,TASK_NAME VARCHAR2(300)
	,TASK_DESCRIPTION VARCHAR2(2000)
	,TASK_START_DATE DATE
	,TASK_END_DATE DATE
	,TASK_COST NUMBER
	,SOURCE_PARENT_ID NUMBER
	,TASK_TYPE VARCHAR2(100)
	,QUANTITY NUMBER );
CREATE OR REPLACE TYPE test_PRJ_TASK_TAB_TYPE IS TABLE OF test_PRJ_TASK_OBJ_TYPE;
CREATE OR REPLACE PROCEDURE test_CREATE_TASK_ACTIVITY (
    p_create_task_i IN test_PRJ_TASK_TAB_TYPE,
	p_project_id_i IN NUMBER) IS BEGIN NULL; END;`
	ctx, cancel := context.WithTimeout(testContext("Issue134"), 10*time.Second)
	defer cancel()
	cx, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cx.Close()
	for _, qry := range strings.Split(crea, ";\n") {
		if strings.HasSuffix(qry, " END") {
			qry += ";"
		}
		if _, err := cx.ExecContext(ctx, qry); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", qry, err))
		}
	}
	defer cleanup()

	conn, err := godror.DriverConn(ctx, cx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ot, err := conn.GetObjectType("TEST_PRJ_TASK_TAB_TYPE")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := ot.NewObject()
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	t.Logf("obj=%#v", obj)
	qry := "BEGIN test_create_task_activity(:1, :2); END;"
	if err := prepExec(ctx, conn, qry,
		driver.NamedValue{Value: &obj, Ordinal: 1},
		driver.NamedValue{Value: 1, Ordinal: 3},
	); err != nil {
		t.Error(fmt.Errorf("%s [%#v, 1]: %w", qry, obj, err))
	}
}

func TestDateRset(t *testing.T) {
	defer tl.enableLogging(t)()
	ctx := testContext("DateRset")

	const qry = `DECLARE
  v_cur SYS_REFCURSOR;
BEGIN
  OPEN v_cur FOR
    SELECT TO_DATE('2015/05/15 8:30:25', 'YYYY/MM/DD HH:MI:SS') as DD FROM DUAL
    UNION ALL SELECT TO_DATE('2015/05/15 8:30:25', 'YYYY/MM/DD HH:MI:SS') as DD FROM DUAL;
  :1 := v_cur;
END;`

	for i := 0; i < maxSessions/2; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			stmt, err := testDb.PrepareContext(ctx, qry)
			if err != nil {
				t.Fatalf("%s: %+v", qry, err)
			}
			defer stmt.Close()

			var rows1 driver.Rows
			if _, err := stmt.ExecContext(ctx, sql.Out{Dest: &rows1}); err != nil {
				t.Fatalf("%s: %+v", qry, err)

			}
			defer rows1.Close()
			cols1 := rows1.(driver.Rows).Columns()
			values := make([]driver.Value, len(cols1))

			var rowNum int
			for {
				rowNum++
				if err := rows1.Next(values); err != nil {
					if err == io.EOF {
						break
					}
				}
				t.Logf("%[1]d. %[2]T %[2]v", rowNum, values[0])
			}
		})
	}
}

func TestTsRset(t *testing.T) {
	defer tl.enableLogging(t)()
	ctx := testContext("TsRset")

	const qry = `DECLARE
  v_cur SYS_REFCURSOR;
BEGIN
  OPEN v_cur FOR
	SELECT TO_TIMESTAMP_TZ('2019-05-01 09:39:12 01:00', 'YYYY-MM-DD HH24:MI:SS TZH:TZM') FROM DUAL
	UNION ALL SELECT FROM_TZ(TO_TIMESTAMP('2019-05-01 09:39:12', 'YYYY-MM-DD HH24:MI:SS'), '01:00') FROM DUAL;
  :1 := v_cur;
END;`

	for i := 0; i < maxSessions/2; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			var rows1 driver.Rows
			if _, err := testDb.ExecContext(ctx, qry, sql.Out{Dest: &rows1}); err != nil {
				t.Fatalf("%s: %+v", qry, err)

			}
			defer rows1.Close()
			cols1 := rows1.(driver.Rows).Columns()
			values := make([]driver.Value, len(cols1))

			var rowNum int
			for {
				rowNum++
				if err := rows1.Next(values); err != nil {
					if err == io.EOF {
						break
					}
				}
				t.Logf("%[1]d. %[2]T %[2]v", rowNum, values[0])
			}
		})
	}
}

func TestTsTZ(t *testing.T) {
	t.Parallel()
	fields := []string{
		"FROM_TZ(TO_TIMESTAMP('2019-05-01 09:39:12', 'YYYY-MM-DD HH24:MI:SS'), '{{.TZ}}')",
		"TO_TIMESTAMP_TZ('2019-05-01 09:39:12 {{.TZ}}', 'YYYY-MM-DD HH24:MI:SS {{.TZDec}}')",
		"CAST(TO_TIMESTAMP_TZ('2019-05-01 09:39:12 {{.TZ}}', 'YYYY-MM-DD HH24:MI:SS {{.TZDec}}') AS DATE)",
	}
	ctx, cancel := context.WithTimeout(testContext("TsTZ"), 10*time.Second)
	defer cancel()

	defer tl.enableLogging(t)()

	for _, Case := range []struct {
		TZ, TZDec string
	}{
		{"00:00", "TZH:TZM"},
		{"01:00", "TZH:TZM"},
		{"-01:00", "TZH:TZM"},
		{"Europe/Berlin", "TZR"},
	} {
		repl := strings.NewReplacer("{{.TZ}}", Case.TZ, "{{.TZDec}}", Case.TZDec)
		for i, f := range fields {
			f = repl.Replace(f)
			qry := "SELECT DUMP(" + f + ") FROM DUAL"
			var s string
			if err := testDb.QueryRowContext(ctx, qry).Scan(&s); err != nil {
				if Case.TZDec != "TZR" {
					t.Fatalf("%s: %s: %+v", Case.TZ, qry, err)
				}
				t.Logf("%s: %s: %+v", Case.TZ, qry, err)
			}
			t.Logf("%s: DUMP[%d]: %q", Case.TZ, i, s)

			qry = "SELECT " + f + " FROM DUAL"
			var ts time.Time
			if err := testDb.QueryRowContext(ctx, qry).Scan(&ts); err != nil {
				var oerr *godror.OraErr
				if errors.As(err, &oerr) && oerr.Code() == 1805 {
					t.Skipf("%s: %s: %+v", Case.TZ, qry, err)
					continue
				}
				t.Fatalf("%s: %s: %+v", Case.TZ, qry, err)
			}
			t.Logf("%s: %d: %s", Case.TZ, i, ts)
		}
	}

	qry := "SELECT filename, version FROM v$timezone_file"
	rows, err := testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Log(qry, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var fn, ver string
		if err := rows.Scan(&fn, &ver); err != nil {
			t.Log(qry, err)
			continue
		}
		t.Log(fn, ver)
	}
	t.Skip("wanted non-zero time")
}

func TestGetDBTimeZone(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()

	ctx, cancel := context.WithTimeout(testContext("GetDBTimeZone"), 10*time.Second)
	defer cancel()
	tx, err := testDb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	qry := "ALTER SESSION SET time_zone = 'UTC'"
	if _, err := tx.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	qry = "SELECT DBTIMEZONE, SESSIONTIMEZONE, SYSTIMESTAMP||'', LOCALTIMESTAMP||'' FROM DUAL"
	var dbTz, tz, sts, lts string
	if err := tx.QueryRowContext(ctx, qry).Scan(&dbTz, &tz, &sts, &lts); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	t.Log("db timezone:", dbTz, "session timezone:", tz, "systimestamp:", sts, "localtimestamp:", lts)

	today := time.Now().Truncate(time.Second)
	for i, tim := range []time.Time{today, today.AddDate(0, 6, 0)} {
		t.Log("local:", tim.Format(time.RFC3339))

		qry = "SELECT :1 FROM DUAL"
		var dbTime time.Time
		if err := tx.QueryRowContext(ctx, qry, tim).Scan(&dbTime); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", qry, err))
		}
		t.Log("db:", dbTime.Format(time.RFC3339))
		if !dbTime.Equal(tim) {
			msg := fmt.Sprintf("db says %s, local is %s", dbTime.Format(time.RFC3339), tim.Format(time.RFC3339))
			if i == 0 {
				t.Error("ERROR:", msg)
			} else {
				t.Log(msg)
			}
		}
	}
}

func TestNumberBool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("NumberBool"), 3*time.Second)
	defer cancel()
	const qry = "SELECT 181 id, 1 status FROM DUAL"
	rows, err := testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}

	for rows.Next() {
		var id int
		var status bool
		if err := rows.Scan(&id, &status); err != nil {
			t.Errorf("failed to scan data: %s\n", err)
		}
		t.Logf("Source id=%d, status=%t\n", id, status)
	}
}

func TestCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skip cancel test")
	}
	db, err := sql.Open("godror", testConStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pid := os.Getpid()
	const maxConc = maxSessions / 2
	db.SetMaxOpenConns(maxConc - 1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithCancel(testContext("Cancel"))
	defer cancel()
	const qryCount = "SELECT COUNT(0) FROM v$session WHERE username = USER AND process = TO_CHAR(:1)"
	cntStmt, err := testDb.PrepareContext(ctx, qryCount)
	if err != nil {
		t.Fatalf("%q: %v", qryCount, err)
	}
	// Just a test, as Prepared statements does not work
	cntStmt.Close()
	Cnt := func() int {
		var cnt int
		if err := db.QueryRowContext(ctx, qryCount, pid).Scan(&cnt); err != nil {
			if strings.Contains(err.Error(), "ORA-00942:") {
				t.Skip(err.Error())
			} else {
				t.Fatal(fmt.Errorf("%s: %w", qryCount, err))
			}
		}
		return cnt
	}

	t.Log("Pid:", pid)
	goal := Cnt() + 1
	t.Logf("Before: %d", goal)
	const qry = "BEGIN FOR rows IN (SELECT 1 FROM DUAL) LOOP DBMS_LOCK.SLEEP(10); END LOOP; END;"
	subCtx, subCancel := context.WithTimeout(ctx, (2*maxConc+1)*time.Second)
	grp, grpCtx := errgroup.WithContext(subCtx)
	for i := 0; i < maxConc; i++ {
		grp.Go(func() error {
			if _, err := db.ExecContext(grpCtx, qry); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("%s: %w", qry, err)
			}
			return nil
		})
	}
	time.Sleep(time.Second)
	t.Logf("After exec, before cancel: %d", Cnt())
	subCancel()
	t.Logf("After cancel: %d", Cnt())
	if err = grp.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	t.Logf("After finish: %d; goal: %d", Cnt(), goal)

	for i := 0; i < 2*maxConc; i++ {
		cnt := Cnt()
		t.Logf("After %ds: %d", i+1, cnt)
		if cnt <= goal {
			return
		}
		time.Sleep(time.Second)
	}
	t.Skip("cancelation timed out")
}

func TestObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext("Object"), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	testCon, err := godror.DriverConn(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer testCon.Close()
	if err = testCon.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("dbstats: %#v", testDb.Stats())

	cleanup := func() {
		testDb.Exec("DROP PROCEDURE test_obj_modify")
		testDb.Exec("DROP TYPE test_obj_tab_t")
		testDb.Exec("DROP TYPE test_obj_rec_t")
	}
	cleanup()
	const crea = `
CREATE OR REPLACE TYPE test_obj_rec_t AS OBJECT (num NUMBER, vc VARCHAR2(1000), dt DATE);
CREATE OR REPLACE TYPE test_obj_tab_t AS TABLE OF test_obj_rec_t;
CREATE OR REPLACE PROCEDURE test_obj_modify(p_obj IN OUT NOCOPY test_obj_tab_t) IS
BEGIN
  p_obj.EXTEND;
  p_obj(p_obj.LAST) := test_obj_rec_t(
    num => 314/100 + p_obj.COUNT,
    vc  => 'abraka dabra',
    dt  => SYSDATE);
END;`
	for _, qry := range strings.Split(crea, "\nCREATE OR") {
		if qry == "" {
			continue
		}
		qry = "CREATE OR" + qry
		if _, err = testDb.ExecContext(ctx, qry); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", qry, err))
		}
	}

	defer cleanup()

	cOt, err := testCon.GetObjectType(strings.ToUpper("test_obj_tab_t"))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(cOt)

	// create object from the type
	coll, err := cOt.NewCollection()
	if err != nil {
		t.Fatal("NewCollection:", err)
	}
	defer coll.Close()

	// create an element object
	elt, err := cOt.CollectionOf.NewObject()
	if err != nil {
		t.Fatal("collection.NewObject:", err)
	}
	defer elt.Close()

	// append to the collection
	t.Logf("append an empty %s", elt)
	coll.AppendObject(elt)

	const mod = "BEGIN test_obj_modify(:1); END;"
	if err = prepExec(ctx, testCon, mod, driver.NamedValue{Ordinal: 1, Value: coll}); err != nil {
		t.Error(err)
	}
	t.Logf("coll: %s", coll)
	var data godror.Data
	for i, err := coll.First(); err == nil; i, err = coll.Next(i) {
		if err = coll.GetItem(&data, i); err != nil {
			t.Fatal(err)
		}
		elt = data.GetObject()

		t.Logf("elt[%d] : %s", i, elt)
		for attr := range elt.Attributes {
			val, err := elt.Get(attr)
			if err != nil {
				t.Error(err, attr)
			}
			t.Logf("elt[%d].%s=%s", i, attr, val)
		}
	}
}

func TestNewPassword(t *testing.T) {
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testContext("NewPassword"), 10*time.Second)
	defer cancel()
	const user, oldPassword, newPassword = "test_expired", "oldPassw0rd_long", "newPassw0rd_longer"

	testDb.Exec("DROP USER " + user)
	// GRANT CREATE USER, DROP USER TO test
	// GRANT CREATE SESSION TO test WITH ADMIN OPTION
	qry := "CREATE USER " + user + " IDENTIFIED BY " + oldPassword + " PASSWORD EXPIRE"
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		if strings.Contains(err.Error(), "ORA-01031:") {
			t.Log("Please issue this:\nGRANT CREATE USER, DROP USER TO " + P.Username + ";\n" +
				"GRANT CREATE SESSION TO " + P.Username + " WITH ADMIN OPTION;\n")
			t.Skip(err)
		}
		t.Fatal(err)
	}
	defer testDb.Exec("DROP USER " + user)

	qry = "GRANT CREATE SESSION TO " + user
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		if strings.Contains(err.Error(), "ORA-01031:") {
			t.Log("Please issue this:\nGRANT CREATE SESSION TO " + P.Username + " WITH ADMIN OPTION;\n")
		}
		t.Fatal(err)
	}

	P.Username, P.Password = user, godror.NewPassword(oldPassword)
	P.StandaloneConnection = true
	P.NewPassword = godror.NewPassword(newPassword)
	{
		db, err := sql.Open("godror", P.StringWithPassword())
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}

	P.Password = P.NewPassword
	P.NewPassword.Reset()
	{
		db, err := sql.Open("godror", P.StringWithPassword())
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestConnClass(t *testing.T) {
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testContext("ConnClass"), 10*time.Second)
	defer cancel()

	const connClass = "fc8153b840"
	P.ConnClass = connClass

	db, err := sql.Open("godror", P.StringWithPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const qry = "SELECT username,program,cclass_name FROM v$cpool_conn_info"
	rows, err := db.QueryContext(ctx, qry)
	if err != nil {
		var oerr *godror.OraErr
		if errors.As(err, &oerr) && oerr.Code() == 942 {
			t.Skip(err)
		}
		t.Fatalf("%s: %+v", qry, err)
	}
	defer rows.Close()

	for rows.Next() {
		var usr, prg, class string
		if err = rows.Scan(&usr, &prg, &class); err != nil {
			t.Fatal(err)
		}
		t.Log(usr, prg, class)
	}
}

func TestOnInit(t *testing.T) {
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	const numChars = "#@"
	P.SetSessionParamOnInit("nls_numeric_characters", numChars)
	t.Log(P.String())
	ctx, cancel := context.WithTimeout(testContext("OnInit"), 10*time.Second)
	defer cancel()

	db, err := sql.Open("godror", P.StringWithPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const qry = "SELECT value, TO_CHAR(123/10) AS num FROM v$nls_parameters WHERE parameter = 'NLS_NUMERIC_CHARACTERS'"
	var v, n string
	if err = db.QueryRowContext(ctx, qry).Scan(&v, &n); err != nil {
		t.Error(err)
	}
	t.Logf("v=%q n=%q", v, n)
	if v != numChars {
		t.Errorf("got %q wanted %q", v, numChars)
	}
	if n != "12#3" {
		t.Errorf("got %q wanted 12#3", n)
	}
}

func TestSelectTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext("SelectTypes"), time.Minute)
	defer cancel()
	const createQry = `CREATE TABLE TEST_TYPES (
			A_LOB BFILE,
			B BINARY_DOUBLE,
			C BINARY_FLOAT,
			D_LOB BLOB,
			E CHAR(10),
			F_LOB CLOB,
			G DATE,
			GN DATE,
			H NUMBER(18 , 2),
			I FLOAT(126),
			J FLOAT(10),
			K NUMBER(38 , 0),
			L INTERVAL DAY TO SECOND(6),
			M INTERVAL YEAR TO MONTH,
			--N LONG,
			P NCHAR(100),
			Q_LOB NCLOB,
			R NUMBER(18 , 2),
			S NUMBER(18 , 2),
			T NVARCHAR2(100),
			U RAW(100),
			V FLOAT(63),
			W NUMBER(38 , 0),
			X TIMESTAMP,
			Y TIMESTAMP WITH LOCAL TIME ZONE,
			Z TIMESTAMP WITH TIME ZONE,
			AA VARCHAR2(100),
			AB XMLTYPE
		)`
	testDb.ExecContext(ctx, "DROP TABLE test_types")
	if _, err := testDb.ExecContext(ctx, createQry); err != nil {
		t.Fatalf("%s: %+v", createQry, err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(testContext("SelectTypes-drop"), 10*time.Second)
		defer cancel()
		testDb.ExecContext(ctx, "DROP TABLE test_types")
	}()

	const insertQry = `INSERT INTO test_types
  (b, c, e, g, gn,
   h, i, j,
   k, l,
   r, s, u,
   v, w, x, y,
   z,
   aa)
  VALUES (3.14, 4.15, 'char(10)', TO_DATE('2020-01-21 09:16:36', 'YYYY-MM-DD HH24:MI:SS'), NULL,
          1/3, 5.16, 6.17,
          123456789012345678901234567890, INTERVAL '8' HOUR,
		  7.18, 8.19, HEXTORAW('deadbeef'),
		  0.01, -3, SYSTIMESTAMP, SYSTIMESTAMP,
		  TIMESTAMP '2018-02-15 14:00:00.00 CET',
          'varchar2(100)')`

	if _, err := testDb.ExecContext(ctx, insertQry); err != nil {
		t.Fatalf("%s: %+v", insertQry, err)
	}
	const insertQry2 = `INSERT INTO test_types (z) VALUES (cast(TO_TIMESTAMP_TZ('2018-02-15T14:00:00 01:00','yyyy-mm-dd"T"hh24:mi:ss TZH:TZM') as date))`
	if _, err := testDb.ExecContext(ctx, insertQry2); err != nil {
		t.Fatalf("%s: %+v", insertQry2, err)
	}
	var n int32
	if err := testDb.QueryRowContext(ctx, "SELECT COUNT(0) FROM test_types").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows in the table, wanted 2", n)
	}

	var dbTZ string
	if err := testDb.QueryRowContext(ctx, "SELECT dbtimezone FROM DUAL").Scan(&dbTZ); err != nil {
		t.Fatal(err)
	}
	rows, err := testDb.QueryContext(ctx, "SELECT filename, version FROM v$timezone_file")
	if err != nil {
		t.Log(err)
	} else {
		for rows.Next() {
			var fn, ver string
			if err = rows.Scan(&fn, &ver); err != nil {
				t.Fatal(err)
			}
			t.Logf("v$timezone file=%q version=%q", fn, ver)
		}
		rows.Close()
	}

	const qry = "SELECT * FROM test_types"

	// get rows
	rows, err = testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Fatalf("%s: %+v", qry, err)
	}
	defer rows.Close()

	// get columns name
	colsName, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("columns:", colsName)

	// get types of query columns
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}

	// total columns
	totalColumns := len(colsName)

	oracleFieldParse := func(datatype string, field interface{}) interface{} {
		// DEBUG: print the type of field and datatype returned for the driver
		t.Logf("%T\t%s\n", field, datatype)

		if field == nil {
			return nil
		}
		switch x := field.(type) {
		case string:
			return x
		case godror.Number:
			return string(x)
		case []uint8:
			switch datatype {
			case "RAW", "LONG RAW":
				return fmt.Sprintf("%x", x)
			default:
				return fmt.Sprintf("unsupported datatype %s", datatype)
			}
		case godror.NullTime:
			if !x.Valid {
				return "NULL"
			}
			if x.Time.IsZero() {
				t.Errorf("zero NullTime.Time, and Valid!?")
			}
			return x.Time.Format(time.RFC3339)
		case time.Time:
			return x.Format(time.RFC3339)
		default:
			return fmt.Sprintf("%v", field)
		}
	}

	// create a slice of interface{}'s to represent each column,
	// and a second slice to contain pointers to each item in the columns slice
	columns := make([]interface{}, totalColumns)
	recordPointers := make([]interface{}, totalColumns)
	for i := range columns {
		if types[i].DatabaseTypeName() == "DATE" {
			var t godror.NullTime
			recordPointers[i] = &t
			columns[i] = t
		} else {
			recordPointers[i] = &columns[i]
		}
	}

	dumpRows := func() {
		dests := make([]string, 0, len(colsName))
		params := make([]interface{}, 0, cap(dests))
		var dumpQry strings.Builder
		dumpQry.WriteString("SELECT ")
		for _, col := range colsName {
			if strings.HasSuffix(col, "_LOB") {
				continue
			}
			if len(dests) != 0 {
				dumpQry.WriteString(", ")
			}
			fmt.Fprintf(&dumpQry, "'%[1]s='||DUMP(%[1]s, 1017,1,10) AS %[1]s", col)
			dests = append(dests, "")
			params = append(params, &dests[len(dests)-1])
		}
		dumpQry.WriteString(" FROM test_types")
		qry := dumpQry.String()
		rows, err := testDb.QueryContext(ctx, qry)
		if err != nil {
			t.Errorf("%s: %+v", qry, err)
			return
		}
		defer rows.Close()
		var i int
		for rows.Next() {
			if err = rows.Scan(params...); err != nil {
				t.Fatal(err)
			}
			i++
			t.Logf("%d. %q", i, dests)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
	}

	// record destination
	record := make([]interface{}, totalColumns)
	var rowCount int
	for rows.Next() {
		// Scan the result into the record pointers...
		if err := rows.Scan(recordPointers...); err != nil {
			dumpRows()
			t.Fatal(err)
		}
		rowCount++

		// Parse each field of recordPointers for get a custom field depending the type
		for idxCol := range recordPointers {
			record[idxCol] = oracleFieldParse(types[idxCol].DatabaseTypeName(), reflect.ValueOf(recordPointers[idxCol]).Elem().Interface())
		}

		t.Log(record)
	}
	if err = rows.Err(); err != nil {
		var cErr interface{ Code() int }
		if errors.As(err, &cErr) && cErr.Code() == 1805 {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	dumpRows()
	if rowCount != 2 {
		t.Errorf("read %d rows, wanted 2", rowCount)
	}
}

func TestInsertIntervalDS(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("InsertIntervalDS"), 10*time.Second)
	defer cancel()
	const tbl = "test_interval_ds"
	testDb.ExecContext(ctx, "DROP TABLE "+tbl)
	qry := "CREATE TABLE " + tbl + " (F_interval_ds INTERVAL DAY TO SECOND(3))"
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer func() { testDb.ExecContext(testContext("InsertIntervalDS-drop"), "DROP TABLE "+tbl) }()

	qry = "INSERT INTO " + tbl + " (F_interval_ds) VALUES (INTERVAL '32' SECOND)"
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	qry = "INSERT INTO " + tbl + " (F_interval_ds) VALUES (:1)"
	if _, err := testDb.ExecContext(ctx, qry, 33*time.Second); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}

	qry = "SELECT F_interval_ds FROM " + tbl + " ORDER BY 1"
	rows, err := testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()
	var got []time.Duration
	for rows.Next() {
		var dur time.Duration
		if err = rows.Scan(&dur); err != nil {
			t.Fatal(err)
		}
		got = append(got, dur)
	}
	t.Log("got:", got)
	if !(len(got) == 2 && got[0] == 32*time.Second && got[1] == 33*time.Second) {
		t.Errorf("wanted [32s, 33s], got %v", got)
	}
}
func TestBool(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(testContext("Bool"), 10*time.Second)
	defer cancel()
	const tbl = "test_bool_t"
	testDb.ExecContext(ctx, "DROP TABLE "+tbl)
	qry := "CREATE TABLE " + tbl + " (F_bool VARCHAR2(1))"
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer func() { testDb.ExecContext(testContext("Bool-drop"), "DROP TABLE "+tbl) }()

	qry = "INSERT INTO " + tbl + " (F_bool) VALUES ('Y')"
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	qry = "INSERT INTO " + tbl + " (F_bool) VALUES (:1)"
	if _, err := testDb.ExecContext(ctx, qry, booler(true)); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	b2s := godror.BoolToString("t", "f")
	if _, err := testDb.ExecContext(ctx, qry, true, b2s); err != nil {
		t.Fatal(err)
	}
	if _, err := testDb.ExecContext(ctx, qry, false, b2s); err != nil {
		t.Fatal(err)
	}

	qry = "SELECT F_bool, F_bool FROM " + tbl + " A ORDER BY ASCII(A.F_bool)"
	rows, err := testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()
	var got []bool
	for rows.Next() {
		var b booler
		var s string
		if err = rows.Scan(&s, &b); err != nil {
			t.Fatal(err)
		}
		t.Logf("%q: %v", s, b)
		got = append(got, bool(b))
	}
	t.Log("got:", got)
	want := []bool{true, true, false, true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wanted %v got %v", want, got)
	}
}

type booler bool

func (b *booler) Scan(src interface{}) error {
	switch x := src.(type) {
	case int:
		*b = x == 1
	case string:
		*b = x == "Y" || x == "t"
	default:
		return fmt.Errorf("unknown scanner source %T", src)
	}
	return nil
}
func (b booler) Value() (driver.Value, error) {
	if b {
		return "Y", nil
	}
	return "N", nil
}

func TestResetSession(t *testing.T) {
	const poolSize = 4
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	P.MinSessions, P.SessionIncrement, P.MaxSessions = 0, 1, poolSize
	db, err := sql.Open("godror", P.StringWithPassword())
	if err != nil {
		t.Fatalf("%s: %+v", P, err)
	}
	defer db.Close()
	db.SetMaxIdleConns(poolSize)

	ctx, cancel := context.WithTimeout(testContext("ResetSession"), time.Minute)
	defer cancel()
	for i := 0; i < 2*poolSize; i++ {
		shortCtx, shortCancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := db.Conn(shortCtx)
		if err != nil {
			shortCancel()
			t.Fatalf("%d. Conn: %+v", i, err)
		}
		err = conn.PingContext(shortCtx)
		shortCancel()
		if err != nil {
			t.Fatalf("%d. Ping: %+v", i, err)
		}
		conn.Close()
	}
}

func TestSelectNullTime(t *testing.T) {
	t.Parallel()
	const qry = "SELECT SYSDATE, SYSDATE+NULL, SYSDATE+NULL FROM DUAL"
	var t0, t1 time.Time
	var nt sql.NullTime
	ctx, cancel := context.WithTimeout(testContext("SelectNullTime"), time.Second)
	err := testDb.QueryRowContext(ctx, qry, godror.NullDateAsZeroTime()).Scan(&t0, &t1, &nt)
	cancel()
	if err != nil {
		t.Fatalf("%+v", err)
	}
	t.Logf("t0=%s t1=%s nt=%v", t0, t1, nt)
}
func TestSelectROWID(t *testing.T) {
	t.Parallel()
	P, err := godror.ParseConnString("user=system password=oracle connectString=\"(DESCRIPTION=(ADDRESS_LIST=(ADDRESS=(PROTOCOL=TCP)(HOST=192.168.56.65)(PORT=1521)))(CONNECT_DATA=(SERVER=DEDICATED)(SERVICE_NAME=orcl)))\"\nconfigDir= connectionClass=godror enableEvents=0 heterogeneousPool=0 libDir=\nnewPassword= poolIncrement=0 poolMaxSessions=0 poolMinSessions=0 poolSessionMaxLifetime=0s\npoolSessionTimeout=0s poolWaitTimeout=0s prelim=0 standaloneConnection=0 sysasm=0\nsysdba=0 sysoper=0 timezone=local")
	if err != nil {
		t.Fatal(err)
	}
	Q, _ := godror.ParseConnString(testConStr)
	P.Username, P.ConnectString = Q.Username, Q.ConnectString
	P.Password.CopyFrom(Q.Password)
	t.Log(P)
	db := sql.OpenDB(godror.NewConnector(P))
	ctx, cancel := context.WithTimeout(testContext("ROWID"), 10*time.Second)
	defer cancel()
	const tbl = "test_rowid_t"
	db.ExecContext(ctx, "DROP TABLE "+tbl)
	qry := "CREATE TABLE " + tbl + " (F_seq NUMBER(6))"
	if _, err := db.ExecContext(ctx, qry); err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer func() { testDb.ExecContext(testContext("ROWID-drop"), "DROP TABLE "+tbl) }()

	qry = "INSERT INTO " + tbl + " (F_seq) VALUES (:1)"
	for i := 0; i < 10; i++ {
		if _, err := db.ExecContext(ctx, qry, i); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", qry, err))
		}
	}
	qry = "SELECT F_seq, ROWID FROM " + tbl + " ORDER BY F_seq"
	rows, err := db.QueryContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", qry, err))
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var rowid string
		if err = rows.Scan(&i, &rowid); err != nil {
			t.Fatalf("scan: %+v", err)
		}
		t.Logf("%d. %v", i, rowid)
		if len(rowid) != 18 {
			t.Errorf("%d. got %v (%d bytes), wanted sth 18 bytes", i, rowid, len(rowid))
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCloseLob(t *testing.T) {
	const poolSize = 2
	P, err := godror.ParseDSN(testConStr)
	if err != nil {
		t.Fatal(err)
	}
	P.WaitTimeout = 5 * time.Second
	P.MinSessions, P.SessionIncrement, P.MaxSessions = 0, 1, poolSize
	t.Log(P.String())
	db, err := sql.Open("godror", P.StringWithPassword())
	if err != nil {
		t.Fatalf("%s: %+v", P, err)
	}
	defer db.Close()
	if P.StandaloneConnection {
		db.SetMaxIdleConns(poolSize)
	} else {
		db.SetMaxOpenConns(0)
		db.SetMaxIdleConns(0)
	}

	ctx, cancel := context.WithTimeout(testContext("OpenCloseLob"), time.Minute)
	defer cancel()

	const qry = "DECLARE v_lob BLOB; BEGIN DBMS_LOB.CREATETEMPORARY(v_lob, TRUE); DBMS_LOB.WRITEAPPEND(v_lob, 4, HEXTORAW('DEADBEEF')); :1 := v_lob; END;"
	for i := 0; i < 10*poolSize; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			shortCtx, shortCancel := context.WithTimeout(ctx, 3*time.Second)
			defer shortCancel()
			tx, err := db.BeginTx(shortCtx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			stmt, err := tx.PrepareContext(shortCtx, qry)
			if err != nil {
				t.Fatalf("%s: %v", qry, err)
			}
			defer stmt.Close()
			var lob godror.Lob
			if _, err = stmt.ExecContext(shortCtx, sql.Out{Dest: &lob}); err != nil {
				t.Error(err)
				return
			}
			b, err := ioutil.ReadAll(lob)
			if err != nil {
				t.Error(err)
				return
			}
			t.Logf("0x%x", b)
		})
	}
}

func TestPreFetchQuery(t *testing.T) {

	if os.Getenv("GODROR_TEST_SYSTEM_USERNAME") == "" ||
		(os.Getenv("GODROR_TEST_SYSTEM_PASSWORD") == "") {
		t.Skip("Please define GODROR_TEST_SYSTEM_USERNAME and GODROR_TEST_SYSTEM_PASSWORD env variables")
	}
	var err error
	if testSystemDb, err = sql.Open("godror", testSystemConStr); err != nil {
		panic(fmt.Errorf("%s: %+v", testConStr, err))
	}

	ctx, cancel := context.WithTimeout(testContext("TestPreFetchQuery"), 30*time.Second)
	defer cancel()

	// Create a table used for Prefetch, ArrayFetch queries

	tbl := "t_employees" + tblSuffix
	testDb.ExecContext(ctx, "DROP TABLE "+tbl)
	if _, err := testDb.ExecContext(ctx, "CREATE TABLE "+tbl+" (employee_id NUMBER)"); err != nil {
		t.Fatal(err)
	}
	defer testDb.Exec("DROP TABLE " + tbl)

	const num = 250 // 250 rows to be created
	nums := make([]godror.Number, num)
	for i := range nums {
		nums[i] = godror.Number(strconv.Itoa(i))
	}

	tx, err := testDb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, tc := range []struct {
		Name  string
		Value interface{}
	}{
		{"employee_id", nums},
	} {
		res, execErr := tx.ExecContext(ctx,
			"INSERT INTO "+tbl+" ("+tc.Name+") VALUES (:1)", //nolint:gas
			tc.Value)
		if execErr != nil {
			t.Fatal("%d. INSERT INTO "+tbl+" (%q) VALUES (%+v): %#v", //nolint:gas
				i, tc.Name, tc.Value, execErr)
		}
		ra, raErr := res.RowsAffected()
		if raErr != nil {
			t.Error(raErr)
		} else if ra != num {
			t.Errorf("%d. %q: wanted %d rows, got %d", i, tc.Name, num, ra)
		}
	}
	tx.Commit()
	sid := func() uint {
		var sid uint
		sql := "SELECT sys_context('userenv','sid') FROM dual"
		err := testDb.QueryRow(sql).Scan(&sid)
		if err != nil {
			t.Fatal(err)
		}
		return sid
	}

	// verify round trips for SingleRowFetch and MultiRowFetch
	// and return failure on unexpected roundtrips

	for _, tCase := range []struct {
		pf, as   int
		srt, mrt uint
	}{
		{useDefaultFetchValue, useDefaultFetchValue, 1, 4},
		{0, useDefaultFetchValue, 2, 4},
		{1, useDefaultFetchValue, 2, 4},
		{2, useDefaultFetchValue, 1, 4},
		{100, useDefaultFetchValue, 1, 3},
		{-1, 100, 2, 4},
		{0, 100, 2, 4},
		{1, 100, 2, 4},
		{2, 100, 1, 4},
		{100, 100, 1, 3},
		{useDefaultFetchValue, 40, 1, 7},
		{2, 40, 1, 7},
		{-1, 40, 2, 7},
		{120, useDefaultFetchValue, 1, 3},
		{120, 100, 1, 3},
		{120, 0, 1, 3},
		{120, -1, 1, 3},
		{120, 250, 1, 2},
		{0, 10, 2, 23},
		{10, 10, 1, 22},
		{214, 214, 1, 2},
		{215, 214, 1, 1},
		{215, useDefaultFetchValue, 1, 1},
		{215, 10, 1, 1},
	} {
		srt, mrt := runPreFetchTests(t, sid(), tCase.pf, tCase.as)
		if !(srt == tCase.srt && mrt == tCase.mrt) {
			t.Fatalf("wanted %d/%d SingleFetchRoundTrip / MultiFetchRoundTrip, got %d/%d", tCase.srt, tCase.mrt, srt, mrt)
		}
	}
}

func runPreFetchTests(t *testing.T, sid uint, pf int, as int) (uint, uint) {
	rt1 := getRoundTrips(t, sid)

	var r uint
	// Do some work
	r = singleRowFetch(t, pf, as)

	rt2 := getRoundTrips(t, sid)

	t.Log("SingleRowFetch: ", "Prefetch:", pf, ", Arraysize:", as, ", Rows: ", r, ", Round-trips:", rt2-rt1)
	srt := rt2 - rt1
	rt1 = rt2
	// Do some work
	r = multiRowFetch(t, pf, as)
	rt2 = getRoundTrips(t, sid)
	t.Log("MultiRowFetch: ", "Prefetch:", pf, ", Arraysize:", as, ", Rows: ", r, ", Round-trips:", rt2-rt1)
	mrt := rt2 - rt1
	return srt, mrt
}
func getRoundTrips(t *testing.T, sid uint) uint {

	sql := `SELECT ss.value
        FROM v$sesstat ss, v$statname sn
        WHERE ss.sid = :sid
        AND ss.statistic# = sn.statistic#
        AND sn.name LIKE '%roundtrip%client%'`
	var rt uint
	err := testSystemDb.QueryRow(sql, sid).Scan(&rt)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func singleRowFetch(t *testing.T, pf int, as int) uint {
	ctx, cancel := context.WithTimeout(testContext("Singlerowfetch"), 10*time.Second)
	defer cancel()
	var employeeid int
	var err error
	tbl := "t_employees" + tblSuffix
	query := "select employee_id from " + tbl + " where employee_id = :id"

	if pf == useDefaultFetchValue && as == useDefaultFetchValue {
		err = testDb.QueryRowContext(ctx, query, 100).Scan(&employeeid)
	} else if pf == useDefaultFetchValue && as != useDefaultFetchValue {
		err = testDb.QueryRowContext(ctx, query, 100, godror.FetchArraySize(as)).Scan(&employeeid)
	} else if pf != useDefaultFetchValue && as == useDefaultFetchValue {
		err = testDb.QueryRowContext(ctx, query, 100, godror.PrefetchCount(pf)).Scan(&employeeid)
	} else {
		err = testDb.QueryRowContext(ctx, query, 100, godror.PrefetchCount(pf), godror.FetchArraySize(as)).Scan(&employeeid)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 1
}

func multiRowFetch(t *testing.T, pf int, as int) uint {

	ctx, cancel := context.WithTimeout(testContext("Singlerowfetch"), 10*time.Second)
	defer cancel()
	tbl := "t_employees" + tblSuffix
	query := "select employee_id from " + tbl + " where rownum < 215"
	var rows *sql.Rows
	var err error

	if pf == useDefaultFetchValue && as == useDefaultFetchValue {
		rows, err = testDb.QueryContext(ctx, query)
	} else if pf == useDefaultFetchValue && as != useDefaultFetchValue {
		rows, err = testDb.QueryContext(ctx, query, godror.FetchArraySize(as))
	} else if pf != useDefaultFetchValue && as == useDefaultFetchValue {
		rows, err = testDb.QueryContext(ctx, query, godror.PrefetchCount(pf))
	} else {
		rows, err = testDb.QueryContext(ctx, query, godror.PrefetchCount(pf), godror.FetchArraySize(as))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var /* employee_id,*/ c uint
	for rows.Next() {
		c++
	}
	err = rows.Err()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssue100(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext("Issue100"), 1*time.Minute)
	defer cancel()
	const baseName = "test_issue100"
	{
		const qry = `create or replace type ` + baseName + `_t as table of TIMESTAMP`
		if _, err := testDb.ExecContext(ctx, qry); err != nil {
			t.Fatalf("%s: %+v", qry, err)
		}
		defer func() { testDb.ExecContext(context.Background(), "DROP TYPE "+baseName+"_t") }()
	}
	{
		const qry = `create or replace FUNCTION ` + baseName + `_f (
    ITERS IN VARCHAR2 DEFAULT 10,
    PAUSE IN VARCHAR2 DEFAULT 1
) RETURN ` + baseName + `_t AUTHID CURRENT_USER PIPELINED AS
BEGIN
    FOR i IN 1..ITERS LOOP
        DBMS_SESSION.SLEEP(PAUSE);
        PIPE ROW(SYSTIMESTAMP);
    END LOOP;
END;`
		if _, err := testDb.ExecContext(ctx, qry); err != nil {
			t.Fatalf("%s: %+v", qry, err)
		}
		defer func() { testDb.ExecContext(context.Background(), "DROP FUNCTION "+baseName+"_f") }()
	}

	const qry = `SELECT * FROM TABLE(` + baseName + `_f(iters => 15, pause => 1))`
	ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	rows, err := testDb.QueryContext(ctx, qry)
	t.Logf("error: %+v", err)
	if err == nil {
		defer rows.Close()
	}

	if ctx.Err() == context.DeadlineExceeded {
		t.Logf("Error: Timeout")
	}
	if rows == nil {
		return
	}

	var res string
	err = rows.Scan(&res)
	if err != nil {
		t.Logf("Row Fetch Error: %+v", err)
	}

	t.Logf("Result: %s", res)
}

func TestStmtFetchDeadlineForLOB(t *testing.T) {

	ctx, cancel := context.WithTimeout(testContext("TestStmtFetchDeadline"), 30*time.Second)
	defer cancel()

	// Create a table used for fetching clob, blob data

	tbl := "t_lob_fetch" + tblSuffix
	const basename = "test_stmtfetchdeadline"
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "DROP TABLE "+tbl)

	conn.ExecContext(ctx,
		"CREATE TABLE "+tbl+" (k number, c clob, b blob)")
	defer testDb.Exec("DROP TABLE " + tbl)

	stmt, err := conn.PrepareContext(ctx,
		"INSERT INTO "+tbl+" (k, c, b) VALUES (:1, :2, :3)", //nolint:gas
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	for tN, tC := range []struct {
		Bytes  []byte
		String string
	}{
		{[]byte{0, 1, 2, 3, 4}, "abcdef"},
		{[]byte{5, 6, 7, 8, 9}, "ghijkl"},
	} {

		// Straight bind
		if _, err := stmt.ExecContext(ctx, tN*2, tC.String, tC.Bytes); err != nil {
			t.Errorf("%d. %v", tN, err)
		}

		if _, err := stmt.ExecContext(ctx, tN*2+1, tC.String, tC.Bytes); err != nil {
			t.Errorf("%d. %v", tN, err)
		}
	}

	const qryf = `create or replace FUNCTION ` + basename + `_f (
    ITERS IN VARCHAR2 DEFAULT 10,
    PAUSE IN VARCHAR2 DEFAULT 1
) RETURN number
IS
cnt number;

BEGIN
    FOR i IN 1..ITERS LOOP
        DBMS_SESSION.SLEEP(PAUSE);
    END LOOP;
    cnt :=2;
    RETURN cnt;
END;`
	if _, err := testDb.ExecContext(context.Background(), qryf); err != nil {
		t.Fatal(fmt.Errorf("%s: %+v", qryf, err))
	}
	defer func() { testDb.ExecContext(context.Background(), "DROP FUNCTION "+tbl+"_f") }()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)

	defer cancel()

	qry := `select k, c, b from ` + tbl + ` where k <= ` + basename + `_f(iters => 2, pause => 1)`
	rows, err := testDb.QueryContext(ctx, qry)
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %v", qry, err))
	}
	defer rows.Close()

	var k int
	var c string
	var b []byte

	for rows.Next() { // stmtFetch wont complete and cause deadline error
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Scan(&k, &c, &b); err != nil {
			t.Fatal(err)
		}
		t.Logf("key=%v, CLOB=%q BLOB=%q\n", k, c, b)
	}

	err = rows.Err()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Logf("Info: %+v", err)
		}
	} else {
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatal("Error:Deadline Not Exceeded")
		}
	}
}
