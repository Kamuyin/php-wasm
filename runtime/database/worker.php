<?php
/* Private embedded database worker. Runs in its own WASM interpreter, with only
 * /backend (read-only) and instance-private /data and /tmp capabilities.
 * The application never loads these classes or mounts these files. */
require __DIR__ . '/mysql/load.php';

const MAX_QUERY_BYTES = 262144;
const MAX_RESULT_BYTES = 3145728;
const MAX_ROWS = 10000;
const MAX_CONNECTIONS = 16;

/** Lex only: use complete token boundaries for policy and placeholder rewriting.
 * MySQL execution uses the separately pinned upstream parser and translator. */
function tokens(string $sql, bool $backslash = true): array {
    $result = []; $offset = 0; $length = strlen($sql);
    while ($offset < $length) {
        $start = $offset; $c = $sql[$offset];
        if (ctype_space($c)) { while ($offset < $length && ctype_space($sql[$offset])) $offset++; $kind = 'space'; }
        elseif (substr($sql,$offset,2)==='--' || $c==='#') { $offset = strpos($sql,"\n",$offset) ?: $length; $kind='comment'; }
        elseif (substr($sql,$offset,2)==='/*') {
            $end=strpos($sql,'*/',$offset+2); if ($end===false) throw new RuntimeException('Unclosed SQL comment');
            if (substr($sql,$offset,3)==='/*!') throw new RuntimeException('Executable SQL comments are unavailable');
            $offset=$end+2;$kind='comment';
        } elseif ($c==="'" || $c==='"' || $c==='`') {
            $quote=$c;$offset++;$closed=false;
            while ($offset<$length) {
                if ($backslash && $sql[$offset]==='\\') { $offset+=2;continue; }
                if ($sql[$offset]===$quote) {
                    if ($offset+1<$length && $sql[$offset+1]===$quote) { $offset+=2;continue; }
                    $offset++;$closed=true;break;
                }
                $offset++;
            }
            if (!$closed) throw new RuntimeException('Unclosed SQL quote');
            $kind=($quote==="'" || ($backslash && $quote==='"'))?'string':'identifier';
        } elseif (preg_match('/\G:[A-Za-z_][A-Za-z_0-9]*/A',$sql,$match,0,$offset)) { $offset+=strlen($match[0]);$kind='parameter'; }
        elseif (preg_match('/\G\$[0-9]+/A',$sql,$match,0,$offset)) { $offset+=strlen($match[0]);$kind='parameter'; }
        elseif (preg_match('/\G[A-Za-z_][A-Za-z_0-9$]*/A',$sql,$match,0,$offset)) { $offset+=strlen($match[0]);$kind='word'; }
        elseif (preg_match('/\G[0-9]+(?:\.[0-9]+)?/A',$sql,$match,0,$offset)) { $offset+=strlen($match[0]);$kind='number'; }
        else { $offset++;$kind='symbol'; }
        $result[]=['start'=>$start,'length'=>$offset-$start,'kind'=>$kind,'text'=>substr($sql,$start,$offset-$start)];
        if (count($result)>32768) throw new RuntimeException('SQL token limit exceeded');
    }
    return $result;
}

function checkQuery(string $sql, string $driver): array {
    if (strlen($sql)>MAX_QUERY_BYTES) throw new RuntimeException('SQL statement exceeds the size limit');
    $list=tokens($sql,$driver==='mysql');$meaningful=array_values(array_filter($list,fn($t)=>$t['kind']!=='space' && $t['kind']!=='comment'));
    if (!$meaningful) throw new RuntimeException('Empty SQL statement');
    foreach ($meaningful as $i=>$token) {
        $word=strtolower(trim($token['text'],'`"'));
        if ($token['kind']==='string') {
            $previous=strtolower($meaningful[$i-1]['text']??'');
            if (in_array($previous,['from','join','into','update','table','.']) || ($meaningful[$i+1]['text']??'')==='(') throw new RuntimeException('String literal cannot name a database object');
            continue;
        }
        if (in_array($word,['attach','detach','pragma','vacuum','load_extension','readfile','writefile','load_file','outfile','dumpfile','copy','lo_import','lo_export','pg_read_file','pg_ls_dir','dblink'])) throw new RuntimeException('Database file or external access is unavailable');
        if (str_starts_with($word,'pragma_') || str_starts_with($word,'sqlite_') || str_starts_with($word,'_wp_sqlite_')) throw new RuntimeException('Unknown database object');
        if ($word===';' && $i!==count($meaningful)-1) throw new RuntimeException('Multiple statements require separate calls');
        if ($driver==='pgsql' && ($word==='::' || ($word===':' && ($meaningful[$i+1]['text']??'')===':'))) throw new RuntimeException('PostgreSQL casts are not supported by this compatibility profile');
    }
    if ($driver==='mysql' && strtoupper($meaningful[0]['text'])==='SET') {
        foreach($meaningful as $i=>$token) if(strtolower(trim($token['text'],'`"'))==='sql_mode') {
            $value=$meaningful[$i+2]??[];
            if (($meaningful[$i+1]['text']??'')!=='=' || ($value['kind']??'')!=='string' ||
                preg_match('/[^A-Za-z_,]/',substr($value['text'],1,-1)) ||
                preg_match('/NO_BACKSLASH_ESCAPES|ANSI_QUOTES|ANSI/i',$value['text']) ||
                (isset($meaningful[$i+3]) && $meaningful[$i+3]['text']!==';')) throw new RuntimeException('SQL mode expression is unavailable');
        }
    }
    return $list;
}

function decodeValue($value) {
    if (is_array($value) && isset($value['bytes'])) {
        $decoded=base64_decode($value['bytes'],true);if ($decoded===false) throw new RuntimeException('Invalid binary parameter');return $decoded;
    }
    if (is_scalar($value) || $value===null) return $value;
    throw new RuntimeException('Unsupported parameter type');
}
function encodeValue($value) { return is_string($value)?['bytes'=>base64_encode($value)]:$value; }

function sqlEvidence(array $list, array $ranges, bool $executed, string $driver): array {
    // A quote that began in trusted SQL is closed by request-derived bytes,
    // followed by request-derived SQL syntax. Escaped strings and bound values
    // do not satisfy this structural proof. The server's parser must also have
    // accepted and executed the statement before the verdict is confirmed.
    foreach (array_slice($ranges,0,64) as $range) {
        if (($range['origin']??'')!=='request' || !empty($range['escaped'])) continue;
        $start=(int)($range['start']??-1);$end=$start+(int)($range['length']??0);
        if ($start<0 || $end<=$start) continue;
        $crossed=false;$syntax=[];
        foreach ($list as $token) {
            $last=$token['start']+$token['length']-1;
            if ($token['kind']==='string' && $token['start']<$start && $last>=$start && $last<$end) $crossed=true;
            if ($crossed && $token['start']>=$start && $last<$end &&
                (($token['kind']==='word' && in_array(strtoupper($token['text']),['OR','AND','UNION','SELECT','DROP','UPDATE','DELETE','INSERT'])) || $token['text']===';')) $syntax[]=$token['text'];
        }
        if ($crossed && $syntax) return ['class'=>'sql_injection','stage'=>$executed?'statement_executed':'attempted','exploit_verdict'=>$executed?'confirmed':'unconfirmed','evidence'=>'request-derived bytes closed a trusted SQL literal and introduced executable SQL syntax','source'=>$range['source']??'unknown','syntax'=>array_slice($syntax,0,8),'driver'=>$driver];
    }
    return [];
}

/** Substitute only lexer-recognized placeholders, never text inside a literal,
 * identifier or comment. Values use the translator's SQL-mode-aware quoting. */
function bindMysql(array $list, array $params, PDO $db): string {
    $sql='';$position=0;$used=[];
    foreach ($list as $token) {
        if ($token['text']==='?' && $token['kind']==='symbol') $key=$position++;
        elseif ($token['kind']==='parameter' && str_starts_with($token['text'],':')) $key=$token['text'];
        else { $sql.=$token['text'];continue; }
        if (!array_key_exists($key,$params) && is_string($key) && array_key_exists(substr($key,1),$params)) $key=substr($key,1);
        if (!array_key_exists($key,$params)) throw new RuntimeException('Missing bound parameter');
        $used[$key]=true;$value=decodeValue($params[$key]);
        $sql.=$value===null?'NULL':(is_bool($value)?($value?'1':'0'):(is_int($value)?(string)$value:$db->quote((string)$value)));
    }
    if (count($used)!==count($params)) throw new RuntimeException('Unused bound parameter');
    return $sql;
}

function executeRequest(array $request, array &$connections, int &$next): array {
    $op=$request['op']??'';
    if ($op==='reset') { foreach($connections as $connection) { if($connection['sqlite']->inTransaction())$connection['sqlite']->rollBack(); } unset($connection);$connections=[];gc_collect_cycles();$next=1;return ['ok'=>true]; }
    if ($op==='connect') {
        if (count($connections)>=MAX_CONNECTIONS) throw new RuntimeException('Too many connections');
        $driver=$request['driver']??'';
        if (!in_array($driver,['mysql','pgsql'],true)) throw new RuntimeException('Unsupported database driver');
        $name=$request['database']??'wordpress';
        if (!is_string($name) || !preg_match('/^[A-Za-z0-9_]{1,64}$/D',$name)) throw new RuntimeException('Invalid database name');
        $filename='/data/'.$driver.'-'.hash('sha256',$name).'.sqlite';
        if (!is_file($filename) && count(glob('/data/*.sqlite'))>=4) throw new RuntimeException('Database count limit reached');
        $sqlite=new PDO('sqlite:'.$filename,null,null,[PDO::ATTR_ERRMODE=>PDO::ERRMODE_EXCEPTION,PDO::ATTR_TIMEOUT=>2]);
        $sqlite->exec('PRAGMA journal_mode=DELETE');$sqlite->exec('PRAGMA synchronous=FULL');
        $sqlite->exec('PRAGMA max_page_count=16384');$sqlite->exec('PRAGMA temp_store=MEMORY');
        if ($driver==='mysql') {
            $db=new WP_MySQL_On_SQLite('mysql-on-sqlite:dbname='.$name,null,null,['sqlite_pdo'=>$sqlite,'sqlite_journal_mode'=>'DELETE','sqlite_synchronous'=>'FULL']);
        } else {
            $db=$sqlite;
            $db->sqliteCreateFunction('version',fn()=>'PostgreSQL 16.4 on x86_64-pc-linux-gnu',0);
            $db->sqliteCreateFunction('current_database',fn()=>$name,0);
        }
        $id=$next++;$connections[$id]=['db'=>$db,'sqlite'=>$sqlite,'driver'=>$driver,'statements'=>[]];
        return ['ok'=>true,'connection'=>$id,'server_version'=>$driver==='mysql'?'8.0.38':'16.4'];
    }
    $id=(int)($request['connection']??0);
    if (!isset($connections[$id])) throw new RuntimeException('Connection is closed');
    if ($op==='close') { if($connections[$id]['sqlite']->inTransaction())$connections[$id]['sqlite']->rollBack();unset($connections[$id]);gc_collect_cycles();return ['ok'=>true]; }
    $connection=&$connections[$id];$db=$connection['db'];$driver=$connection['driver'];
    if ($op==='quote') return ['ok'=>true,'quoted'=>base64_encode($db->quote(decodeValue($request['value']??'')))];
    $sql=base64_decode($request['sql_b64']??'',true);if ($sql===false) throw new RuntimeException('Invalid SQL encoding');
    $list=checkQuery($sql,$driver);
    if ($driver==='pgsql') {
        $translated='';
        foreach ($list as $token) {
            if ($token['kind']==='parameter' && str_starts_with($token['text'],'$')) $translated.=':p'.substr($token['text'],1);
            elseif ($token['kind']==='word' && strtoupper($token['text'])==='SERIAL') {
                if (!preg_match('/^\s+PRIMARY\s+KEY\b/i',substr($sql,$token['start']+$token['length']))) throw new RuntimeException('SERIAL requires PRIMARY KEY in this compatibility profile');
                $translated.='INTEGER';
            }
            elseif ($token['kind']==='word' && strtoupper($token['text'])==='ILIKE') throw new RuntimeException('ILIKE is not supported by this compatibility profile');
            else $translated.=$token['text'];
        }
        $sql=$translated;
    }
    if ($op==='prepare') {
        // Keep query text in the application handle; connection-private prepared
        // state is reconstructed on execute, and is reset at the request boundary.
        return ['ok'=>true];
    }
    if ($op!=='query') throw new RuntimeException('Unsupported database operation');
    $params=$request['params']??[];
    if (!is_array($params)) throw new RuntimeException('Invalid parameters');
    $executed=false;
    try {
    if ($driver==='mysql') {
        $statement=$db->query(bindMysql($list,$params,$db));$executed=true;
    } else {
    $numbered=(bool)array_filter($list,fn($t)=>$t['kind']==='parameter' && str_starts_with($t['text'],'$'));
    $used=[];$position=0;$styles=[];
    foreach ($list as $token) {
        if ($token['kind']==='parameter' && str_starts_with($token['text'],'$')) {
            $key=(int)substr($token['text'],1)-1;$styles['numbered']=true;
            if ($key<0) throw new RuntimeException('Invalid numbered parameter');
        } elseif ($token['kind']==='parameter') {
            $key=$token['text'];$styles['named']=true;
            if (!array_key_exists($key,$params) && array_key_exists(substr($key,1),$params)) $key=substr($key,1);
        } elseif ($token['kind']==='symbol' && $token['text']==='?') {
            $key=$position++;$styles['positional']=true;
        } else continue;
        if (!array_key_exists($key,$params)) throw new RuntimeException('Missing bound parameter');
        $used[$key]=true;
    }
    if (count($styles)>1 || count($used)!==count($params)) throw new RuntimeException('Mixed or unused parameters');
    $statement=$db->prepare($sql);
    foreach ($params as $key=>$value) {
        $value=decodeValue($value);
        $parameter=is_int($key)?($numbered?':p'.($key+1):$key+1):$key;
        $type=$value===null?PDO::PARAM_NULL:(is_int($value)?PDO::PARAM_INT:(is_bool($value)?PDO::PARAM_BOOL:PDO::PARAM_STR));
        $statement->bindValue($parameter,$value,$type);
    }
        $statement->execute();$executed=true;
    }
        $columns=[];
        for ($i=0;$i<$statement->columnCount();$i++) {
            $meta=$statement->getColumnMeta($i)?:[];
            $columns[]=['name'=>$meta['name']??(string)$i,'type'=>$meta['native_type']??'VAR_STRING','table'=>$meta['table']??'','flags'=>$meta['flags']??[],'length'=>$meta['len']??0];
        }
        $rows=[];$bytes=0;
        while (($row=$statement->fetch(PDO::FETCH_NUM))!==false) {
            if (count($rows)>=MAX_ROWS) throw new RuntimeException('Result row limit exceeded');
            $encoded=array_map('encodeValue',$row);$bytes+=strlen(json_encode($encoded,JSON_THROW_ON_ERROR));
            if ($bytes>MAX_RESULT_BYTES) throw new RuntimeException('Result byte limit exceeded');
            $rows[]=$encoded;
        }
        $result=['ok'=>true,'columns'=>$columns,'rows'=>$rows,'affected_rows'=>$statement->rowCount(),'insert_id'=>$db->lastInsertId()];
        $proof=sqlEvidence($list,$request['provenance']??[],$executed,$driver);
        if ($proof) $result['evidence']=$proof;
        return $result;
    } catch (Throwable $error) {
        $proof=sqlEvidence($list,$request['provenance']??[],$executed,$driver);
        // Preserve whether execution occurred even if result collection hit a cap.
        $constraint=str_contains(strtolower($error->getMessage()),'unique constraint');
        $state=$constraint?($driver==='pgsql'?'23505':'23000'):($driver==='pgsql'?'42601':'42000');
        return ['ok'=>false,'errno'=>$constraint?1062:1064,'sqlstate'=>$state,'error'=>$constraint?'Duplicate key value':'Database statement failed or exceeded a resource limit','evidence'=>$proof,'executed'=>$executed];
    }
}

$connections=[];$next=1;
$input=fopen('php://stdin','rb');$output=fopen('php://stdout','wb');
fwrite($output,"{\"ready\":1}\n");fflush($output);
while (!feof($input)) {
    $line=fgets($input,1048578);if ($line===false) break;
    if (!str_ends_with($line,"\n")) break;
    try {
        $request=json_decode($line,true,32,JSON_THROW_ON_ERROR);
        if (!is_array($request)) throw new RuntimeException('Invalid request');
        $response=executeRequest($request,$connections,$next);
    } catch (Throwable $error) {
        $response=['ok'=>false,'errno'=>1064,'sqlstate'=>'HY000','error'=>'Unsupported or invalid database operation',
            'evidence'=>['class'=>'database_compatibility','stage'=>'operation_unavailable','exploit_verdict'=>'unconfirmed','evidence'=>'database operation rejected by the compatibility profile','operation'=>$request['op']??'unknown','query_b64'=>substr($request['sql_b64']??'',0,16384)]];
        // Diagnostic details stay in this private worker, never in PHP-visible
        // database errors, SQL result metadata, or the application's filesystem.
    }
    $encoded=json_encode($response,JSON_INVALID_UTF8_SUBSTITUTE|JSON_THROW_ON_ERROR);
    fwrite($output,$encoded."\n");fflush($output);
}
