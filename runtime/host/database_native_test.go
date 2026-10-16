package runtimehost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func databaseWorker(t *testing.T) func(context.Context, api.Module, json.RawMessage) ([]byte, error) {
	t.Helper()
	binary, backend := os.Getenv("PHP_WASM_ORDINARY_BINARY"), os.Getenv("PHP_WASM_DATABASE_BACKEND")
	if binary == "" || backend == "" {
		t.Skip("set PHP_WASM_ORDINARY_BINARY and PHP_WASM_DATABASE_BACKEND")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := r.CompileModule(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("database").WithStdin(inR).WithStdout(outW).WithStderr(os.Stderr).WithEnv("USE_ZEND_ALLOC", "0").WithArgs("php-cgi", "-q", "-n", "-d", "memory_limit=192M", "-f", "/backend/worker.php").WithFSConfig(wazero.NewFSConfig().WithReadOnlyDirMount(backend, "/backend").WithDirMount(t.TempDir(), "/data").WithDirMount(t.TempDir(), "/tmp")))
		outW.Close()
		done <- err
	}()
	t.Cleanup(func() {
		inW.Close()
		outR.Close()
		cancel()
		err := <-done
		var exit *sys.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 0) && !strings.Contains(err.Error(), "context canceled") {
			t.Logf("database shutdown: %v", err)
		}
		r.Close(context.Background())
	})
	scanner := bufio.NewScanner(outR)
	scanner.Buffer(make([]byte, 65536), 4194304)
	if !scanner.Scan() || scanner.Text() != `{"ready":1}` {
		t.Fatal("database startup", scanner.Text(), scanner.Err())
	}
	return func(_ context.Context, _ api.Module, request json.RawMessage) ([]byte, error) {
		if _, err := inW.Write(append(append([]byte(nil), request...), '\n')); err != nil {
			return nil, err
		}
		if !scanner.Scan() {
			return nil, fmt.Errorf("database response: %v", scanner.Err())
		}
		return append([]byte(nil), scanner.Bytes()...), nil
	}
}

func TestNativeDatabaseAPIs(t *testing.T) {
	db := databaseWorker(t)
	out, _ := runPHPDatabase(t, `
mysqli_report(MYSQLI_REPORT_ERROR|MYSQLI_REPORT_STRICT);
$db=mysqli_init();$db->real_connect('localhost','root','secret','wordpress');
$db->query('CREATE TABLE items (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL)');
$s=$db->prepare('INSERT INTO items(name) VALUES (?)');$name="a' OR 1=1 -- ";$s->bind_param('s',$name);$s->execute();
$id=$db->query('SELECT id,name FROM items')->fetch_assoc();
$s=$db->prepare('SELECT name FROM items WHERE id=?');$s->execute([1]);$s->bind_result($bound);$s->fetch();
$p=new PDO('mysql:host=localhost;dbname=wordpress','root','secret');
$p->prepare('INSERT INTO items(name) VALUES (:name)')->execute(['name'=>'second']);
$names=$p->query('SELECT name FROM items ORDER BY id')->fetchAll(PDO::FETCH_COLUMN);
$pg=pg_connect('host=localhost dbname=postgres');
pg_query($pg,'CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT)');
pg_query_params($pg,'INSERT INTO items(name) VALUES ($1)',['first']);
pg_prepare($pg,'find','SELECT name FROM items WHERE id=$1');$row=pg_fetch_assoc(pg_execute($pg,'find',[1]));
$pp=new PDO('pgsql:host=localhost;dbname=postgres');$ps=$pp->prepare('SELECT name FROM items WHERE id=?');$ps->execute([1]);
echo json_encode(['id'=>$id,'bound'=>$bound,'names'=>$names,'pg'=>$row,'pdo_pg'=>$ps->fetchColumn(),'native'=>(new ReflectionFunction('mysqli_query'))->isInternal(),'drivers'=>PDO::getAvailableDrivers()]);`, []Config{{Version: 1}}, false, db)
	var result map[string]any
	if err := json.Unmarshal([]byte(out[strings.Index(out, "\r\n\r\n")+4:]), &result); err != nil {
		t.Fatal(err, out)
	}
	if result["native"] != true || result["bound"] != "a' OR 1=1 -- " || result["pdo_pg"] != "first" {
		t.Fatal(result)
	}
	if result["pg"].(map[string]any)["name"] != "first" || len(result["names"].([]any)) != 2 {
		t.Fatal(result)
	}
}

func TestNativeProvenance(t *testing.T) {
	_, events := runPHP(t, `
$x=$_GET['code'];
eval($x);
eval('/* concatenated */ '.$x);
eval("/* interpolated */ $x");
$y='/* assigned */ ';$y.=$x;eval($y);
eval(substr('x',1).trim($x));
$self=$_GET['code'];$self.=$self;eval($self);
eval('return 40 + 2;');
`, []Config{{Version: 1}}, false)
	var tainted, clean int
	for _, event := range events {
		e := event.Data
		if e["kind"] == "code.compile" && e["code_type"] == "eval" {
			ranges, _ := e["provenance"].([]any)
			if len(ranges) > 0 {
				tainted++
			} else {
				clean++
			}
		}
	}
	if tainted != 6 || clean != 1 {
		t.Fatalf("tainted %d clean %d: %#v", tainted, clean, events)
	}
}

func TestNativeSQLInjectionEvidence(t *testing.T) {
	db := databaseWorker(t)
	out, events := runPHPDatabase(t, `
 $db=new mysqli('localhost','root','','wordpress');
 $db->query('CREATE TABLE accounts (id INT, name VARCHAR(100))');
 $db->query("INSERT INTO accounts VALUES (1,'admin')");
 $q="SELECT id FROM accounts WHERE name='".$_GET['sqli']."'";
 $vulnerable=$db->query($q)->num_rows;
 $safe=$db->prepare('SELECT id FROM accounts WHERE name=?');$safe->execute([$_GET['sqli']]);
 $bound=$safe->get_result()->num_rows;
 $escaped=$db->query("SELECT id FROM accounts WHERE name='".$db->real_escape_string($_GET['sqli'])."'")->num_rows;
 echo json_encode([$vulnerable,$bound,$escaped]);`, []Config{{Version: 1}}, false, db)
	if !strings.HasSuffix(out, "[1,0,0]") {
		t.Fatal(out)
	}
	proofs := 0
	for _, event := range events {
		if event.Data["kind"] == "security.evidence" && event.Data["class"] == "sql_injection" {
			proofs++
			if event.Data["exploit_verdict"] != "confirmed" {
				t.Fatal(event)
			}
		}
	}
	if proofs != 1 {
		t.Fatalf("want one confirmed injection, got %d: %v", proofs, events)
	}
}
func TestNativeReservedObservations(t *testing.T) {
	_, events := runPHP(t, `function auth(){return true;}for($i=0;$i<50;$i++)@file_get_contents('/tmp/absent');auth();eval($_GET['code']);`, []Config{{Version: 1, MaxEvents: 2, Hooks: map[string]Hook{"auth": {After: `return ['emit'=>['event_name'=>'auth.success']];`}}}}, false)
	if !hasEvent(events, "application.observation", "") {
		t.Fatal("application outcome dropped at trace limit")
	}
	found := false
	for _, e := range events {
		if e.Data["kind"] == "code.enter" && e.Data["request_derived"] == true {
			found = true
		}
	}
	if !found {
		t.Fatal("security evidence dropped at trace limit")
	}
}

func TestNativeDatabaseFastCGIIsolation(t *testing.T) {
	db := databaseWorker(t)
	out, events := runPHPDatabase(t, `
try {$db=new mysqli('localhost','root','','invalid-database');}catch(mysqli_sql_exception $e){echo 'strict;';}
mysqli_report(MYSQLI_REPORT_OFF);
$db=new mysqli('localhost','root','','wordpress');
$db->query('CREATE TABLE IF NOT EXISTS isolated (id INT)');
echo 'before='.$db->query('SELECT COUNT(*) AS n FROM isolated')->fetch_assoc()['n'].';';
$db->begin_transaction();$db->query('INSERT INTO isolated VALUES (1)');
echo 'inside='.$db->query('SELECT COUNT(*) AS n FROM isolated')->fetch_assoc()['n'].';';
`, []Config{{Version: 1}, {Version: 1}}, true, db)
	if strings.Count(out, "strict;") != 2 || strings.Count(out, "before=0;inside=1;") != 2 {
		t.Fatal(out)
	}
	if len(events) == 0 {
		t.Fatal("missing native events")
	}
}

func TestNativeDatabaseBindingTypes(t *testing.T) {
	db := databaseWorker(t)
	out, _ := runPHPDatabase(t, `
$db=new mysqli('localhost','root','','binding_types');
$s=$db->prepare('SELECT ? AS integer_value, ? AS decimal_value, ? AS string_value, ? AS null_value');
$i='12x';$d='2.5';$str=42;$nil=null;$s->bind_param('idss',$i,$d,$str,$nil);$s->execute();
$row=$s->get_result()->fetch_assoc();
$rejected=[];
foreach ([['mysql',false],['pgsql',true]] as [$driver,$emulated]) {
 try {new PDO($driver.':dbname=binding_types','','',[PDO::ATTR_EMULATE_PREPARES=>$emulated]);$rejected[]=false;}
 catch (PDOException $e) {$rejected[]=true;}
}
echo json_encode(['row'=>$row,'originals'=>[$i,$d,$str,$nil],'rejected'=>$rejected]);`, []Config{{Version: 1}}, false, db)
	var result struct {
		Row struct {
			Integer string `json:"integer_value"`
			Decimal string `json:"decimal_value"`
			String  string `json:"string_value"`
			Null    any    `json:"null_value"`
		} `json:"row"`
		Originals []any  `json:"originals"`
		Rejected  []bool `json:"rejected"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "\r\n\r\n")+4:]), &result); err != nil {
		t.Fatal(err, out)
	}
	if result.Row.Integer != "12" || result.Row.Decimal != "2.5" || result.Row.String != "42" || result.Row.Null != nil {
		t.Fatal(out)
	}
	if len(result.Rejected) != 2 || !result.Rejected[0] || !result.Rejected[1] || len(result.Originals) != 4 || result.Originals[0] != "12x" || result.Originals[1] != "2.5" || result.Originals[2] != float64(42) || result.Originals[3] != nil {
		t.Fatal(out)
	}
}

func TestNativePostgresParameters(t *testing.T) {
	db := databaseWorker(t)
	out, _ := runPHPDatabase(t, `
$p=new PDO('pgsql:dbname=parameters');
$s=$p->prepare('SELECT :number AS n, :value AS v, :nil AS nil, :value AS repeated');
$s->bindValue(':number',7,PDO::PARAM_INT);$s->bindValue(':value',"a' OR 1=1 -- ");$s->bindValue(':nil',null,PDO::PARAM_NULL);$s->execute();
$row=$s->fetch(PDO::FETCH_ASSOC);
$s=$p->prepare('SELECT :name');$name='first';$s->bindParam(':name',$name);$name='second';$s->execute();$reference=$s->fetchColumn();
$s=$p->prepare('SELECT :name');$s->execute(['name'=>'third']);$named=$s->fetchColumn();
$s=$p->prepare("SELECT ?, '?' AS literal /* ? :ignored $9 */");$s->execute(['fourth']);$positional=$s->fetchColumn();
$rejected=0;
foreach ([['SELECT :missing',[]],['SELECT 1',['extra'=>'value']],['SELECT $0',[1]]] as [$query,$params]) {
 try {$p->prepare($query)->execute($params);}catch(PDOException $e){$rejected++;}
}
echo json_encode([$row,$reference,$named,$positional,$rejected]);`, []Config{{Version: 1}}, false, db)
	var result []any
	if err := json.Unmarshal([]byte(out[strings.Index(out, "\r\n\r\n")+4:]), &result); err != nil {
		t.Fatal(err, out)
	}
	if len(result) != 5 || result[1] != "second" || result[2] != "third" || result[3] != "fourth" || result[4] != float64(3) {
		t.Fatal(out)
	}
	row := result[0].(map[string]any)
	if row["n"] != float64(7) || row["v"] != "a' OR 1=1 -- " || row["repeated"] != row["v"] || row["nil"] != nil {
		t.Fatal(out)
	}
}
