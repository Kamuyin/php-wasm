/* Native database facades. SQL runs in an instance-private embedded worker.
 * This file is included by php_wasm_runtime.c, never exposed as application PHP.
 * Copyright (c) php-wasm contributors. MIT; see repository LICENSE. */
#include "ext/standard/base64.h"
#include "ext/spl/spl_exceptions.h"
#include "ext/pdo/php_pdo.h"
#include "ext/pdo/php_pdo_driver.h"

__attribute__((import_module("php_wasm_runtime_v1"), import_name("database")))
extern int32_t pw_host_database(const char *, int32_t, char *, int32_t);

static zend_class_entry *pw_mysqli_ce, *pw_result_ce, *pw_stmt_ce, *pw_sql_exception_ce;
static zend_class_entry *pw_pg_connection_ce, *pw_pg_result_ce;
static zend_object_handlers pw_db_handlers;
static zend_long pw_mysqli_report = 3, pw_connect_errno;
static zend_string *pw_connect_error;

typedef struct {
    zval owner, params, response, bound_results;
    zend_string *sql, *param_types;
    zend_long connection, row, field;
    bool closed, postgres;
    zend_object std;
} pw_db_object;

static pw_db_object *pw_db_obj(zend_object *object) { return (pw_db_object *)((char *)object - XtOffsetOf(pw_db_object, std)); }
static zend_long pw_db_long(zval *array, const char *key) { zval *v=pw_field(array,key); return v?zval_get_long(v):0; }
static const char *pw_db_text(zval *array, const char *key, const char *fallback) { zval *v=pw_field(array,key); return v && Z_TYPE_P(v)==IS_STRING?Z_STRVAL_P(v):fallback; }
static void pw_db_prop(pw_db_object *o, const char *key, zval *value) { zend_update_property(o->std.ce,&o->std,key,strlen(key),value); }
static void pw_db_prop_long(pw_db_object *o, const char *key, zend_long value) { zend_update_property_long(o->std.ce,&o->std,key,strlen(key),value); }
static void pw_db_prop_string(pw_db_object *o, const char *key, const char *value) { zend_update_property_string(o->std.ce,&o->std,key,strlen(key),value); }

static bool pw_db_exchange(zval *request, zval *response)
{
    smart_str encoded={0}; int saved=JSON_G(error_code);
    zend_result valid=php_json_encode(&encoded,request,0);
    JSON_G(error_code)=saved;
    ZVAL_UNDEF(response);
    if (valid==SUCCESS && encoded.s && ZSTR_LEN(encoded.s)<=1048576) {
        char *buffer=emalloc(4194305);
        int32_t length=pw_host_database(ZSTR_VAL(encoded.s),ZSTR_LEN(encoded.s),buffer,4194304);
        if (length>0 && length<=4194304) {
            buffer[length]=0;
            php_json_decode_ex(response,buffer,length,PHP_JSON_OBJECT_AS_ARRAY,32);
            JSON_G(error_code)=saved;
        }
        efree(buffer);
    }
    smart_str_free(&encoded);
    if (Z_TYPE_P(response)!=IS_ARRAY) {
        if (!Z_ISUNDEF_P(response)) zval_ptr_dtor(response);
        array_init(response);add_assoc_bool(response,"ok",false);
        add_assoc_long(response,"errno",2006);add_assoc_string(response,"sqlstate","HY000");
        add_assoc_string(response,"error","Database connection is unavailable");
    }
    zval *proof=pw_field(response,"evidence");
    if (pw_active && !pw_in_hook && proof && Z_TYPE_P(proof)==IS_ARRAY && zend_hash_num_elements(Z_ARRVAL_P(proof))) {
        zval event;ZVAL_DUP(&event,proof);add_assoc_long(&event,"version",1);add_assoc_string(&event,"kind","security.evidence");
        pw_emit(&event,false);zval_ptr_dtor(&event);
    }
    zval *ok=pw_field(response,"ok"); return ok && zend_is_true(ok);
}

static void pw_db_encode(zval *out,zval *in) {
    ZVAL_DEREF(in);
    if (Z_TYPE_P(in)==IS_STRING) { array_init(out);add_assoc_str(out,"bytes",php_base64_encode((unsigned char *)Z_STRVAL_P(in),Z_STRLEN_P(in))); }
    else if (Z_TYPE_P(in)<=IS_DOUBLE) ZVAL_COPY(out,in);
    else { ZVAL_NULL(out);zend_type_error("Database parameters must be scalar or null"); }
}
static void pw_db_decode(zval *out,zval *in) {
    zval *bytes=pw_field(in,"bytes");
    if (bytes && Z_TYPE_P(bytes)==IS_STRING) { zend_string *s=php_base64_decode_ex((unsigned char *)Z_STRVAL_P(bytes),Z_STRLEN_P(bytes),true);if(s)ZVAL_STR(out,s);else ZVAL_NULL(out); }
    else if(in)ZVAL_COPY(out,in);else ZVAL_NULL(out);
}

static bool pw_db_connect(zend_long *id,bool postgres,const char *database,zval *response) {
    zval request;array_init(&request);add_assoc_string(&request,"op","connect");
    add_assoc_string(&request,"driver",postgres?"pgsql":"mysql");add_assoc_string(&request,"database",(char *)(database && *database?database:"wordpress"));
    bool ok=pw_db_exchange(&request,response);if(ok)*id=pw_db_long(response,"connection");zval_ptr_dtor(&request);return ok;
}
static bool pw_db_query(zend_long id,zend_string *sql,zval *params,bool prepare,zval *response) {
    zval request,values;array_init(&request);array_init(&values);
    add_assoc_string(&request,"op",prepare?"prepare":"query");add_assoc_long(&request,"connection",id);
    add_assoc_str(&request,"sql_b64",php_base64_encode((unsigned char *)ZSTR_VAL(sql),ZSTR_LEN(sql)));
    if (params && Z_TYPE_P(params)==IS_ARRAY) {
        zend_string *key;zend_ulong index;zval *value;
        ZEND_HASH_FOREACH_KEY_VAL(Z_ARRVAL_P(params),index,key,value) {
            zval encoded;pw_db_encode(&encoded,value);
            if(key)zend_hash_update(Z_ARRVAL(values),key,&encoded);else zend_hash_index_update(Z_ARRVAL(values),index,&encoded);
        } ZEND_HASH_FOREACH_END();
    }
    add_assoc_zval(&request,"params",&values);
    /* Native string provenance is attached here, before any SQL translation. */
    pw_string_provenance(&values,sql);add_assoc_zval(&request,"provenance",&values);
    bool ok=!EG(exception) && pw_db_exchange(&request,response);
    if(EG(exception))ZVAL_UNDEF(response);
    zval_ptr_dtor(&request);return ok;
}
static void pw_db_disconnect(zend_long id) {
    if(!id || !pw_active)return;
    zval request,response;array_init(&request);add_assoc_string(&request,"op","close");add_assoc_long(&request,"connection",id);
    pw_db_exchange(&request,&response);zval_ptr_dtor(&request);zval_ptr_dtor(&response);
}

static zend_object *pw_db_create(zend_class_entry *ce) {
    pw_db_object *o=zend_object_alloc(sizeof(*o),ce);
    ZVAL_NULL(&o->owner);array_init(&o->params);array_init(&o->response);array_init(&o->bound_results);
    zend_object_std_init(&o->std,ce);object_properties_init(&o->std,ce);o->std.handlers=&pw_db_handlers;
    o->postgres=ce==pw_pg_connection_ce || ce==pw_pg_result_ce;return &o->std;
}
static void pw_db_free(zend_object *object) {
    pw_db_object *o=pw_db_obj(object);
    if(object->ce==pw_mysqli_ce || object->ce==pw_pg_connection_ce)pw_db_disconnect(o->connection);
    zval_ptr_dtor(&o->owner);zval_ptr_dtor(&o->params);zval_ptr_dtor(&o->response);zval_ptr_dtor(&o->bound_results);
    if(o->sql)zend_string_release(o->sql);if(o->param_types)zend_string_release(o->param_types);zend_object_std_dtor(object);
}
static HashTable *pw_db_gc(zend_object *object,zval **table,int *count) {
    *table=&pw_db_obj(object)->owner;*count=4;return zend_std_get_properties(object);
}
static void pw_db_result(zval *out,pw_db_object *owner,zval *response,bool postgres) {
    object_init_ex(out,postgres?pw_pg_result_ce:pw_result_ce);pw_db_object *o=pw_db_obj(Z_OBJ_P(out));
    zval_ptr_dtor(&o->response);ZVAL_COPY(&o->response,response);ZVAL_OBJ_COPY(&o->owner,&owner->std);
    zval *rows=pw_field(response,"rows"),*cols=pw_field(response,"columns");
    if(!postgres){pw_db_prop_long(o,"num_rows",rows?zend_hash_num_elements(Z_ARRVAL_P(rows)):0);pw_db_prop_long(o,"field_count",cols?zend_hash_num_elements(Z_ARRVAL_P(cols)):0);}
}
static bool pw_db_status(pw_db_object *o,zval *response,bool ok) {
    o->row=o->field=0;
    if(!Z_ISUNDEF_P(response)){zval_ptr_dtor(&o->response);ZVAL_COPY(&o->response,response);}
    if(o->postgres)return ok;
    pw_db_prop_long(o,"errno",ok?0:pw_db_long(response,"errno"));
    pw_db_prop_string(o,"error",ok?"":pw_db_text(response,"error","Database operation failed"));
    pw_db_prop_string(o,"sqlstate",ok?"00000":pw_db_text(response,"sqlstate","HY000"));
    if(ok){pw_db_prop_long(o,"affected_rows",pw_db_long(response,"affected_rows"));pw_db_prop_long(o,"insert_id",pw_db_long(response,"insert_id"));}
    else if(pw_mysqli_report&2)zend_throw_exception(pw_sql_exception_ce,pw_db_text(response,"error","Database operation failed"),pw_db_long(response,"errno"));
    return ok;
}
static void pw_db_fetch(pw_db_object *o,zval *out,int mode,bool object) {
    zval *rows=pw_field(&o->response,"rows"),*columns=pw_field(&o->response,"columns");
    zval *row=rows?zend_hash_index_find(Z_ARRVAL_P(rows),o->row++):NULL;
    if(!row){if(o->postgres)ZVAL_FALSE(out);else ZVAL_NULL(out);return;}
    if(object)object_init(out);else array_init(out);
    zend_ulong i;zval *value;
    ZEND_HASH_FOREACH_NUM_KEY_VAL(Z_ARRVAL_P(row),i,value){
        zval decoded;pw_db_decode(&decoded,value);
        if(Z_TYPE(decoded)!=IS_NULL)convert_to_string(&decoded);
        zval *column=columns?zend_hash_index_find(Z_ARRVAL_P(columns),i):NULL;
        const char *name=pw_db_text(column,"name","");
        if(object)zend_update_property(Z_OBJCE_P(out),Z_OBJ_P(out),name,strlen(name),&decoded);
        else {if(mode&2){zval copy;ZVAL_COPY(&copy,&decoded);zend_hash_index_update(Z_ARRVAL_P(out),i,&copy);}if(mode&1){zval copy;ZVAL_COPY(&copy,&decoded);zend_hash_str_update(Z_ARRVAL_P(out),name,strlen(name),&copy);}}
        zval_ptr_dtor(&decoded);
    }ZEND_HASH_FOREACH_END();
}

ZEND_BEGIN_ARG_INFO_EX(pw_db_args,0,0,0)
    ZEND_ARG_VARIADIC_INFO(0,args)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_INFO_EX(pw_db_bind_args,0,0,2)
    ZEND_ARG_INFO(0,statement)
    ZEND_ARG_INFO(0,types)
    ZEND_ARG_VARIADIC_INFO(1,variables)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_INFO_EX(pw_db_bind_method_args,0,0,1)
    ZEND_ARG_INFO(0,types)
    ZEND_ARG_VARIADIC_INFO(1,variables)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_INFO_EX(pw_db_bind_result_args,0,0,1)
    ZEND_ARG_INFO(0,statement)
    ZEND_ARG_VARIADIC_INFO(1,variables)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_INFO_EX(pw_db_bind_result_method_args,0,0,0)
    ZEND_ARG_VARIADIC_INFO(1,variables)
ZEND_END_ARG_INFO()

static zval *pw_db_arg(zend_execute_data *frame,uint32_t index) { return index<ZEND_CALL_NUM_ARGS(frame)?ZEND_CALL_ARG(frame,index+1):NULL; }
static const char *pw_db_argtext(zend_execute_data *frame,uint32_t index,const char *fallback) { zval *arg=pw_db_arg(frame,index);return arg&&Z_TYPE_P(arg)==IS_STRING?Z_STRVAL_P(arg):fallback; }

static PHP_FUNCTION(pw_mysqli_call)
{
    const char *name=ZSTR_VAL(execute_data->func->common.function_name);bool method=Z_TYPE(execute_data->This)==IS_OBJECT;
    if(!method && !strncmp(name,"mysqli_",7))name+=7;
    if(!method && !strncmp(name,"stmt_",5))name+=5;
    uint32_t base=method?0:1;
    if(!strcmp(name,"report")){zval *arg=pw_db_arg(execute_data,0);if(!arg){zend_argument_count_error("Report flags required");RETURN_THROWS();}pw_mysqli_report=zval_get_long(arg);RETURN_TRUE;}
    if(!strcmp(name,"get_client_info"))RETURN_STRING("mysqlnd 8.3.30");
    if(!strcmp(name,"get_client_version"))RETURN_LONG(80330);
    if(!strcmp(name,"connect_errno"))RETURN_LONG(pw_connect_errno);
    if(!strcmp(name,"connect_error")){if(pw_connect_error)RETURN_STR_COPY(pw_connect_error);RETURN_NULL();}
    if(!strcmp(name,"init")){object_init_ex(return_value,pw_mysqli_ce);return;}
    bool constructing=!strcmp(name,"__construct") || !strcmp(name,"connect");
    zval *receiver=method?&execute_data->This:pw_db_arg(execute_data,0);
    if(constructing && !method){object_init_ex(return_value,pw_mysqli_ce);receiver=return_value;base=0;}
    if(!receiver || Z_TYPE_P(receiver)!=IS_OBJECT || Z_OBJ_HT_P(receiver)!=&pw_db_handlers){zend_type_error("Expected a mysqli connection, statement or result");RETURN_THROWS();}
    pw_db_object *o=pw_db_obj(Z_OBJ_P(receiver));
    if(constructing && method && !ZEND_CALL_NUM_ARGS(execute_data))return;
    if(constructing || !strcmp(name,"real_connect")){
        zval response;const char *database=pw_db_argtext(execute_data,base+3,"wordpress");
        bool ok=pw_db_connect(&o->connection,false,database,&response);
        pw_connect_errno=ok?0:pw_db_long(&response,"errno");
        if(pw_connect_error){zend_string_release(pw_connect_error);pw_connect_error=NULL;}
        if(!ok)pw_connect_error=zend_string_init(pw_db_text(&response,"error","Connection failed"),strlen(pw_db_text(&response,"error","Connection failed")),0);
        pw_db_prop_long(o,"connect_errno",pw_connect_errno);if(pw_connect_error)pw_db_prop_string(o,"connect_error",ZSTR_VAL(pw_connect_error));
        pw_db_status(o,&response,ok);zval_ptr_dtor(&response);
        if(ok){pw_db_prop_string(o,"server_info","8.0.38");pw_db_prop_long(o,"server_version",80038);pw_db_prop_string(o,"host_info","Localhost via UNIX socket");pw_db_prop_long(o,"protocol_version",10);pw_db_prop_long(o,"thread_id",o->connection);pw_db_prop_long(o,"connect_errno",0);}
        if(method && constructing)return;if(!constructing)RETURN_BOOL(ok);if(!ok){zval_ptr_dtor(return_value);RETVAL_FALSE;}return;
    }
    if(!strcmp(name,"close") || !strcmp(name,"free") || !strcmp(name,"free_result")){
        if(o->std.ce==pw_mysqli_ce){pw_db_disconnect(o->connection);o->connection=0;}o->closed=true;RETURN_TRUE;
    }
    if(o->closed){zend_throw_error(NULL,"Database handle is already closed");RETURN_THROWS();}
    if(!strcmp(name,"ping"))RETURN_BOOL(o->connection!=0);
    if(!strcmp(name,"options") || !strcmp(name,"set_opt")){zval *option=pw_db_arg(execute_data,base);RETURN_BOOL(option && (zval_get_long(option)==0 || zval_get_long(option)==7));}
    if(!strcmp(name,"set_charset")){const char *charset=pw_db_argtext(execute_data,base,"");RETURN_BOOL(!strcmp(charset,"utf8mb4") || !strcmp(charset,"utf8") || !strcmp(charset,"latin1"));}
    if(!strcmp(name,"character_set_name"))RETURN_STRING("utf8mb4");
    if(!strcmp(name,"get_server_info"))RETURN_STRING("8.0.38");
    if(!strcmp(name,"get_server_version"))RETURN_LONG(80038);
    if(!strcmp(name,"get_host_info"))RETURN_STRING("Localhost via UNIX socket");
    if(!strcmp(name,"get_proto_info"))RETURN_LONG(10);
    if(!strcmp(name,"more_results") || !strcmp(name,"next_result"))RETURN_FALSE;
    if(!strcmp(name,"select_db")){
        zval response;zend_long previous=o->connection;bool ok=pw_db_connect(&o->connection,false,pw_db_argtext(execute_data,base,"wordpress"),&response);
        if(ok)pw_db_disconnect(previous);pw_db_status(o,&response,ok);zval_ptr_dtor(&response);RETURN_BOOL(ok);
    }
    if(!strcmp(name,"real_escape_string") || !strcmp(name,"escape_string")){
        zval *arg=pw_db_arg(execute_data,base);if(!arg){zend_argument_count_error("String argument required");RETURN_THROWS();}
        zval request,response,value;array_init(&request);add_assoc_string(&request,"op","quote");add_assoc_long(&request,"connection",o->connection);pw_db_encode(&value,arg);add_assoc_zval(&request,"value",&value);
        bool ok=pw_db_exchange(&request,&response);zval *encoded=pw_field(&response,"quoted");
        if(ok && encoded && Z_TYPE_P(encoded)==IS_STRING){zend_string *quoted=php_base64_decode_ex((unsigned char*)Z_STRVAL_P(encoded),Z_STRLEN_P(encoded),true);if(quoted && ZSTR_LEN(quoted)>=2){RETVAL_STRINGL(ZSTR_VAL(quoted)+1,ZSTR_LEN(quoted)-2);}else RETVAL_FALSE;if(quoted)zend_string_release(quoted);}else RETVAL_FALSE;
        zval_ptr_dtor(&request);zval_ptr_dtor(&response);return;
    }
    if(!strcmp(name,"bind_param") || !strcmp(name,"bind_result")){
        bool result=!strcmp(name,"bind_result");uint32_t start=base+(result?0:1);zval *dest=result?&o->bound_results:&o->params;
        if(!result){
            zval *types=pw_db_arg(execute_data,base);
            if(!types || Z_TYPE_P(types)!=IS_STRING || Z_STRLEN_P(types)!=ZEND_CALL_NUM_ARGS(execute_data)-start){zend_value_error("Type string must match bound variable count");RETURN_THROWS();}
            for(size_t i=0;i<Z_STRLEN_P(types);i++)if(!strchr("idsb",Z_STRVAL_P(types)[i])){zend_value_error("Invalid parameter type");RETURN_THROWS();}
            if(o->param_types)zend_string_release(o->param_types);o->param_types=zend_string_copy(Z_STR_P(types));
        }
        zval_ptr_dtor(dest);array_init(dest);
        for(uint32_t i=start;i<ZEND_CALL_NUM_ARGS(execute_data);i++){zval copy;ZVAL_COPY(&copy,ZEND_CALL_ARG(execute_data,i+1));add_next_index_zval(dest,&copy);}RETURN_TRUE;
    }
    if(!strcmp(name,"query") || !strcmp(name,"real_query") || !strcmp(name,"prepare") || !strcmp(name,"execute") || !strcmp(name,"execute_query") || !strcmp(name,"begin_transaction") || !strcmp(name,"commit") || !strcmp(name,"rollback")){
        bool prepared=!strcmp(name,"prepare"),execute=!strcmp(name,"execute");
        zend_string *sql=NULL;zval *params=NULL;
        if(execute){sql=o->sql?zend_string_copy(o->sql):NULL;params=pw_db_arg(execute_data,base);if(!params)params=&o->params;}
        else if(!strcmp(name,"begin_transaction"))sql=zend_string_init("START TRANSACTION",17,0);
        else if(!strcmp(name,"commit"))sql=zend_string_init("COMMIT",6,0);
        else if(!strcmp(name,"rollback"))sql=zend_string_init("ROLLBACK",8,0);
        else {zval *arg=pw_db_arg(execute_data,base);if(arg)sql=zval_try_get_string(arg);if(!strcmp(name,"execute_query"))params=pw_db_arg(execute_data,base+1);}
        if(!sql){zend_argument_count_error("SQL statement required");RETURN_THROWS();}
        zval coerced;ZVAL_UNDEF(&coerced);
        if(execute && params==&o->params && o->param_types){
            array_init(&coerced);zend_ulong i;zval *value;
            ZEND_HASH_FOREACH_NUM_KEY_VAL(Z_ARRVAL(o->params),i,value){zval copy;ZVAL_COPY_DEREF(&copy,value);
                if(Z_TYPE(copy)!=IS_NULL && i<ZSTR_LEN(o->param_types)){char type=ZSTR_VAL(o->param_types)[i];if(type=='i')convert_to_long(&copy);else if(type=='d')convert_to_double(&copy);else convert_to_string(&copy);}
                add_next_index_zval(&coerced,&copy);
            }ZEND_HASH_FOREACH_END();params=&coerced;
        }
        zval response;bool ok=pw_db_query(o->connection,sql,params,prepared,&response);
        if(!Z_ISUNDEF(coerced))zval_ptr_dtor(&coerced);pw_db_status(o,&response,ok);
        if(ok && prepared){object_init_ex(return_value,pw_stmt_ce);pw_db_object *stmt=pw_db_obj(Z_OBJ_P(return_value));stmt->connection=o->connection;stmt->sql=zend_string_copy(sql);ZVAL_OBJ_COPY(&stmt->owner,&o->std);}
        else if(ok){zval *columns=pw_field(&response,"columns");if(!execute && strcmp(name,"real_query") && columns && zend_hash_num_elements(Z_ARRVAL_P(columns)))pw_db_result(return_value,o,&response,false);else RETVAL_TRUE;}
        else RETVAL_FALSE;
        if(!Z_ISUNDEF(response))zval_ptr_dtor(&response);zend_string_release(sql);return;
    }
    if(!strcmp(name,"store_result") || !strcmp(name,"use_result") || !strcmp(name,"get_result")){
        if(o->std.ce==pw_stmt_ce && !strcmp(name,"store_result"))RETURN_TRUE;
        zval *columns=pw_field(&o->response,"columns");if(!columns || !zend_hash_num_elements(Z_ARRVAL_P(columns)))RETURN_FALSE;
        pw_db_result(return_value,o,&o->response,false);return;
    }
    if(!strcmp(name,"fetch_assoc") || !strcmp(name,"fetch_row") || !strcmp(name,"fetch_array") || !strcmp(name,"fetch_object")){
        int mode=!strcmp(name,"fetch_assoc")?1:(!strcmp(name,"fetch_row")?2:3);zval *arg=pw_db_arg(execute_data,base);
        if(!strcmp(name,"fetch_array") && arg)mode=zval_get_long(arg);
        pw_db_fetch(o,return_value,mode,!strcmp(name,"fetch_object"));return;
    }
    if(!strcmp(name,"fetch_all")){
        int mode=2;zval *arg=pw_db_arg(execute_data,base);if(arg)mode=zval_get_long(arg);array_init(return_value);
        while(true){zval row;pw_db_fetch(o,&row,mode,false);if(Z_TYPE(row)==IS_NULL)break;add_next_index_zval(return_value,&row);}return;
    }
    if(!strcmp(name,"fetch")){
        zval row;pw_db_fetch(o,&row,2,false);if(Z_TYPE(row)==IS_NULL)RETURN_NULL();
        zend_ulong i;zval *target;ZEND_HASH_FOREACH_NUM_KEY_VAL(Z_ARRVAL(o->bound_results),i,target){zval *value=zend_hash_index_find(Z_ARRVAL(row),i);if(value)zend_assign_to_variable(target,value,IS_CV,true);}ZEND_HASH_FOREACH_END();zval_ptr_dtor(&row);RETURN_TRUE;
    }
    if(!strcmp(name,"data_seek")){
        zval *arg=pw_db_arg(execute_data,base),*rows=pw_field(&o->response,"rows");zend_long row=arg?zval_get_long(arg):-1;
        if(row<0 || !rows || row>=zend_hash_num_elements(Z_ARRVAL_P(rows)))RETURN_FALSE;o->row=row;RETURN_TRUE;
    }
    if(!strcmp(name,"num_rows")){zval *rows=pw_field(&o->response,"rows");RETURN_LONG(rows?zend_hash_num_elements(Z_ARRVAL_P(rows)):0);}
    if(!strcmp(name,"num_fields") || !strcmp(name,"field_count")){zval *cols=pw_field(&o->response,"columns");RETURN_LONG(cols?zend_hash_num_elements(Z_ARRVAL_P(cols)):0);}
    if(!strcmp(name,"affected_rows") || !strcmp(name,"insert_id") || !strcmp(name,"errno"))RETURN_LONG(pw_db_long(&o->response,name));
    if(!strcmp(name,"error") || !strcmp(name,"sqlstate"))RETURN_STRING(pw_db_text(&o->response,name,!strcmp(name,"sqlstate")?"00000":""));
    if(!strcmp(name,"fetch_field") || !strcmp(name,"fetch_fields") || !strcmp(name,"fetch_field_direct")){
        zval *columns=pw_field(&o->response,"columns");if(!columns)RETURN_FALSE;
        bool all=!strcmp(name,"fetch_fields");if(all)array_init(return_value);
        zend_long count=zend_hash_num_elements(Z_ARRVAL_P(columns)),index=o->field++;
        if(!strcmp(name,"fetch_field_direct")){zval *arg=pw_db_arg(execute_data,base);index=arg?zval_get_long(arg):0;}
        for(zend_long i=all?0:index;i<(all?count:index+1);i++){
            zval *column=zend_hash_index_find(Z_ARRVAL_P(columns),i);if(!column){if(!all)RETVAL_FALSE;break;}
            zval field;object_init(&field);const char *colname=pw_db_text(column,"name","");
            zend_update_property_string(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("name"),colname);zend_update_property_string(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("orgname"),colname);
            zend_update_property_string(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("table"),pw_db_text(column,"table",""));
            zend_update_property_long(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("type"),!strcmp(pw_db_text(column,"type",""),"integer")?3:253);
            zend_update_property_long(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("charsetnr"),45);zend_update_property_long(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("flags"),0);
            zend_update_property_long(Z_OBJCE(field),Z_OBJ(field),ZEND_STRL("length"),pw_db_long(column,"length"));
            if(all)add_next_index_zval(return_value,&field);else ZVAL_COPY_VALUE(return_value,&field);
        }return;
    }
    zend_throw_error(NULL,"This database operation is unavailable");RETURN_THROWS();
}

static PHP_FUNCTION(pw_pg_call)
{
    const char *name=ZSTR_VAL(execute_data->func->common.function_name)+3;
    if(!strcmp(name,"connect") || !strcmp(name,"pconnect")){
        const char *dsn=pw_db_argtext(execute_data,0,"");const char *start=strstr(dsn,"dbname=");char database[65]="postgres";
        if(start){start+=7;char quote=(*start=='\'' || *start=='"')?*start++:0;size_t n=0;while(start[n] && n<64 && (quote?start[n]!=quote:!isspace((unsigned char)start[n])))n++;memcpy(database,start,n);database[n]=0;}
        object_init_ex(return_value,pw_pg_connection_ce);pw_db_object *o=pw_db_obj(Z_OBJ_P(return_value));zval response;
        bool ok=pw_db_connect(&o->connection,true,database,&response);pw_db_status(o,&response,ok);zval_ptr_dtor(&response);
        if(!ok){zval_ptr_dtor(return_value);RETVAL_FALSE;}return;
    }
    zval *receiver=pw_db_arg(execute_data,0);
    if(!receiver || Z_TYPE_P(receiver)!=IS_OBJECT || Z_OBJ_HT_P(receiver)!=&pw_db_handlers){zend_type_error("Expected a PgSql connection or result");RETURN_THROWS();}
    pw_db_object *o=pw_db_obj(Z_OBJ_P(receiver));
    if(!strcmp(name,"close") || !strcmp(name,"free_result")){if(o->std.ce==pw_pg_connection_ce)pw_db_disconnect(o->connection);o->closed=true;o->connection=0;RETURN_TRUE;}
    if(o->closed){zend_throw_error(NULL,"PostgreSQL handle is closed");RETURN_THROWS();}
    if(!strcmp(name,"connection_status"))RETURN_LONG(0);
    if(!strcmp(name,"ping"))RETURN_TRUE;
    if(!strcmp(name,"version")){array_init(return_value);add_assoc_string(return_value,"client","16.4");add_assoc_string(return_value,"server","16.4");add_assoc_long(return_value,"protocol",3);return;}
    if(!strcmp(name,"last_error"))RETURN_STRING(pw_db_text(&o->response,"error",""));
    if(!strcmp(name,"query") || !strcmp(name,"query_params") || !strcmp(name,"prepare") || !strcmp(name,"execute")){
        bool prepare=!strcmp(name,"prepare"),execute=!strcmp(name,"execute");zval *params=NULL;zend_string *sql=NULL;
        if(execute){const char *key=pw_db_argtext(execute_data,1,"");zval *stored=zend_hash_str_find(Z_ARRVAL(o->params),key,strlen(key));if(stored)sql=zval_get_string(stored);params=pw_db_arg(execute_data,2);}
        else {zval *arg=pw_db_arg(execute_data,prepare?2:1);if(arg)sql=zval_try_get_string(arg);if(!strcmp(name,"query_params"))params=pw_db_arg(execute_data,2);}
        if(!sql)RETURN_FALSE;
        zval response;bool ok=pw_db_query(o->connection,sql,params,prepare,&response);pw_db_status(o,&response,ok);
        if(ok && prepare){const char *key=pw_db_argtext(execute_data,1,"");zval value;ZVAL_STR_COPY(&value,sql);zend_hash_str_update(Z_ARRVAL(o->params),key,strlen(key),&value);}
        if(ok)pw_db_result(return_value,o,&response,true);else RETVAL_FALSE;
        if(!Z_ISUNDEF(response))zval_ptr_dtor(&response);zend_string_release(sql);return;
    }
    if(!strcmp(name,"escape_string") || !strcmp(name,"escape_literal") || !strcmp(name,"escape_identifier")){
        zval *argument=pw_db_arg(execute_data,1);if(!argument){zend_argument_count_error("String argument required");RETURN_THROWS();}zend_string *input=zval_try_get_string(argument);if(!input)RETURN_THROWS();smart_str out={0};char quote=!strcmp(name,"escape_identifier")?'"':'\'';
        bool literal=strcmp(name,"escape_string")!=0;if(literal)smart_str_appendc(&out,quote);
        for(size_t i=0;i<ZSTR_LEN(input);i++){if(ZSTR_VAL(input)[i]==quote)smart_str_appendc(&out,quote);smart_str_appendc(&out,ZSTR_VAL(input)[i]);}
        if(literal)smart_str_appendc(&out,quote);smart_str_0(&out);zend_string_release(input);RETURN_STR(out.s?out.s:ZSTR_EMPTY_ALLOC());
    }
    if(!strcmp(name,"fetch_assoc") || !strcmp(name,"fetch_row") || !strcmp(name,"fetch_array") || !strcmp(name,"fetch_object")){
        zval *index=pw_db_arg(execute_data,1);if(index && Z_TYPE_P(index)!=IS_NULL)o->row=zval_get_long(index);
        int mode=!strcmp(name,"fetch_row")?2:(!strcmp(name,"fetch_assoc")?1:3);zval *arg=pw_db_arg(execute_data,2);if(arg && !strcmp(name,"fetch_array"))mode=zval_get_long(arg);
        pw_db_fetch(o,return_value,mode,!strcmp(name,"fetch_object"));return;
    }
    if(!strcmp(name,"fetch_result")){
        zval *index=pw_db_arg(execute_data,1),*field=pw_db_arg(execute_data,2);if(!index || !field)RETURN_FALSE;
        zval *rows=pw_field(&o->response,"rows"),*columns=pw_field(&o->response,"columns");zval *row=rows?zend_hash_index_find(Z_ARRVAL_P(rows),zval_get_long(index)):NULL;
        if(!row)RETURN_FALSE;zend_long column=Z_TYPE_P(field)==IS_LONG?Z_LVAL_P(field):-1;
        if(column<0 && columns && Z_TYPE_P(field)==IS_STRING){zend_ulong i;zval *meta;ZEND_HASH_FOREACH_NUM_KEY_VAL(Z_ARRVAL_P(columns),i,meta){if(!strcmp(pw_db_text(meta,"name",""),Z_STRVAL_P(field))){column=i;break;}}ZEND_HASH_FOREACH_END();}
        zval *value=column>=0?zend_hash_index_find(Z_ARRVAL_P(row),column):NULL;if(!value)RETURN_FALSE;pw_db_decode(return_value,value);if(Z_TYPE_P(return_value)!=IS_NULL)convert_to_string(return_value);return;
    }
    if(!strcmp(name,"num_rows")){zval *rows=pw_field(&o->response,"rows");RETURN_LONG(rows?zend_hash_num_elements(Z_ARRVAL_P(rows)):0);}
    if(!strcmp(name,"num_fields")){zval *cols=pw_field(&o->response,"columns");RETURN_LONG(cols?zend_hash_num_elements(Z_ARRVAL_P(cols)):0);}
    if(!strcmp(name,"affected_rows"))RETURN_LONG(pw_db_long(&o->response,"affected_rows"));
    if(!strcmp(name,"result_status")){zval *cols=pw_field(&o->response,"columns");RETURN_LONG(cols && zend_hash_num_elements(Z_ARRVAL_P(cols))?2:1);}
    if(!strcmp(name,"field_name") || !strcmp(name,"field_type")){
        zval *index=pw_db_arg(execute_data,1),*cols=pw_field(&o->response,"columns");zval *column=index && cols?zend_hash_index_find(Z_ARRVAL_P(cols),zval_get_long(index)):NULL;
        if(!column)RETURN_FALSE;RETURN_STRING(pw_db_text(column,!strcmp(name,"field_name")?"name":"type","text"));
    }
    zend_throw_error(NULL,"This PostgreSQL operation is unavailable");RETURN_THROWS();
}

typedef struct { zend_long connection;bool postgres;zval response; } pw_pdo_db;
typedef struct { zend_string *sql;zval response;zend_long row; } pw_pdo_stmt;

static void pw_pdo_error(pdo_dbh_t *dbh,pdo_stmt_t *stmt,zval *response) {
    pw_pdo_db *db=dbh->driver_data;zval_ptr_dtor(&db->response);ZVAL_COPY(&db->response,response);
    memcpy(stmt?stmt->error_code:dbh->error_code,pw_db_text(response,"sqlstate","HY000"),5);
    (stmt?stmt->error_code:dbh->error_code)[5]=0;
}
static int pw_pdo_stmt_free(pdo_stmt_t *stmt){pw_pdo_stmt *s=stmt->driver_data;if(s){zend_string_release(s->sql);zval_ptr_dtor(&s->response);efree(s);}return 1;}
static int pw_pdo_execute(pdo_stmt_t *stmt){
    pw_pdo_db *db=stmt->dbh->driver_data;pw_pdo_stmt *s=stmt->driver_data;zval params,response;array_init(&params);
    if(stmt->bound_params){struct pdo_bound_param_data *param;ZEND_HASH_FOREACH_PTR(stmt->bound_params,param){
        if(!param->is_param)continue;zval value;ZVAL_COPY_DEREF(&value,&param->parameter);
        if(PDO_PARAM_TYPE(param->param_type)==PDO_PARAM_NULL){zval_ptr_dtor(&value);ZVAL_NULL(&value);}
        else if(PDO_PARAM_TYPE(param->param_type)==PDO_PARAM_INT)convert_to_long(&value);
        else if(PDO_PARAM_TYPE(param->param_type)==PDO_PARAM_BOOL)convert_to_boolean(&value);
        if(param->name)zend_hash_update(Z_ARRVAL(params),param->name,&value);else zend_hash_index_update(Z_ARRVAL(params),param->paramno,&value);
    }ZEND_HASH_FOREACH_END();}
    bool ok=pw_db_query(db->connection,s->sql,&params,false,&response);zval_ptr_dtor(&params);
    if(!ok){if(!Z_ISUNDEF(response)){pw_pdo_error(stmt->dbh,stmt,&response);zval_ptr_dtor(&response);}return 0;}
    zval_ptr_dtor(&s->response);ZVAL_COPY_VALUE(&s->response,&response);s->row=-1;
    zval *columns=pw_field(&s->response,"columns");php_pdo_stmt_set_column_count(stmt,columns?zend_hash_num_elements(Z_ARRVAL_P(columns)):0);
    stmt->row_count=pw_db_long(&s->response,"affected_rows");zval_ptr_dtor(&db->response);ZVAL_COPY(&db->response,&s->response);return 1;
}
static int pw_pdo_fetch(pdo_stmt_t *stmt,enum pdo_fetch_orientation orientation,zend_long offset){
    pw_pdo_stmt *s=stmt->driver_data;zval *rows=pw_field(&s->response,"rows");if(orientation!=PDO_FETCH_ORI_NEXT)return 0;
    return rows && ++s->row<zend_hash_num_elements(Z_ARRVAL_P(rows));
}
static int pw_pdo_describe(pdo_stmt_t *stmt,int index){pw_pdo_stmt *s=stmt->driver_data;zval *cols=pw_field(&s->response,"columns");zval *col=cols?zend_hash_index_find(Z_ARRVAL_P(cols),index):NULL;stmt->columns[index].name=zend_string_init(pw_db_text(col,"name",""),strlen(pw_db_text(col,"name","")),0);stmt->columns[index].maxlen=pw_db_long(col,"length");return 1;}
static int pw_pdo_column(pdo_stmt_t *stmt,int index,zval *out,enum pdo_param_type *type){pw_pdo_stmt *s=stmt->driver_data;zval *rows=pw_field(&s->response,"rows");zval *row=rows?zend_hash_index_find(Z_ARRVAL_P(rows),s->row):NULL;zval *value=row?zend_hash_index_find(Z_ARRVAL_P(row),index):NULL;pw_db_decode(out,value);return 1;}
static int pw_pdo_meta(pdo_stmt_t *stmt,zend_long index,zval *out){pw_pdo_stmt *s=stmt->driver_data;zval *cols=pw_field(&s->response,"columns");zval *col=cols?zend_hash_index_find(Z_ARRVAL_P(cols),index):NULL;if(!col)return FAILURE;ZVAL_DUP(out,col);return SUCCESS;}
static int pw_pdo_cursor_close(pdo_stmt_t *stmt){pw_pdo_stmt *s=stmt->driver_data;zval_ptr_dtor(&s->response);array_init(&s->response);s->row=-1;return 1;}
static const struct pdo_stmt_methods pw_pdo_stmt_methods={.dtor=pw_pdo_stmt_free,.executer=pw_pdo_execute,.fetcher=pw_pdo_fetch,.describer=pw_pdo_describe,.get_col=pw_pdo_column,.get_column_meta=pw_pdo_meta,.cursor_closer=pw_pdo_cursor_close};

static bool pw_pdo_prepare(pdo_dbh_t *dbh,zend_string *sql,pdo_stmt_t *stmt,zval *options){
    pw_pdo_stmt *s=ecalloc(1,sizeof(*s));s->sql=zend_string_copy(sql);s->row=-1;array_init(&s->response);
    stmt->driver_data=s;stmt->methods=&pw_pdo_stmt_methods;stmt->supports_placeholders=PDO_PLACEHOLDER_POSITIONAL|PDO_PLACEHOLDER_NAMED;
    pw_pdo_db *db=dbh->driver_data;zval response;bool ok=pw_db_query(db->connection,sql,NULL,true,&response);
    if(!ok && !Z_ISUNDEF(response))pw_pdo_error(dbh,stmt,&response);if(!Z_ISUNDEF(response))zval_ptr_dtor(&response);return ok;
}
static void pw_pdo_close(pdo_dbh_t *dbh){pw_pdo_db *db=dbh->driver_data;if(db){pw_db_disconnect(db->connection);zval_ptr_dtor(&db->response);efree(db);dbh->driver_data=NULL;}}
static zend_long pw_pdo_exec(pdo_dbh_t *dbh,const zend_string *sql){pw_pdo_db *db=dbh->driver_data;zval response;bool ok=pw_db_query(db->connection,(zend_string*)sql,NULL,false,&response);zend_long count=-1;if(ok){count=pw_db_long(&response,"affected_rows");zval_ptr_dtor(&db->response);ZVAL_COPY(&db->response,&response);}else if(!Z_ISUNDEF(response))pw_pdo_error(dbh,NULL,&response);if(!Z_ISUNDEF(response))zval_ptr_dtor(&response);return count;}
static bool pw_pdo_transaction(pdo_dbh_t *dbh,const char *sql){zend_string *text=zend_string_init(sql,strlen(sql),0);bool ok=pw_pdo_exec(dbh,text)>=0;zend_string_release(text);return ok;}
static bool pw_pdo_begin(pdo_dbh_t *dbh){return pw_pdo_transaction(dbh,((pw_pdo_db*)dbh->driver_data)->postgres?"BEGIN":"START TRANSACTION");}
static bool pw_pdo_commit(pdo_dbh_t *dbh){return pw_pdo_transaction(dbh,"COMMIT");}
static bool pw_pdo_rollback(pdo_dbh_t *dbh){return pw_pdo_transaction(dbh,"ROLLBACK");}
static zend_string *pw_pdo_quote(pdo_dbh_t *dbh,const zend_string *input,enum pdo_param_type type){
    pw_pdo_db *db=dbh->driver_data;zval request,response,value,string;array_init(&request);add_assoc_string(&request,"op","quote");add_assoc_long(&request,"connection",db->connection);
    ZVAL_STR(&string,(zend_string*)input);pw_db_encode(&value,&string);add_assoc_zval(&request,"value",&value);zend_string *result=NULL;
    if(pw_db_exchange(&request,&response)){zval *v=pw_field(&response,"quoted");if(v && Z_TYPE_P(v)==IS_STRING)result=php_base64_decode_ex((unsigned char*)Z_STRVAL_P(v),Z_STRLEN_P(v),true);}
    zval_ptr_dtor(&request);zval_ptr_dtor(&response);return result;
}
static zend_string *pw_pdo_last_id(pdo_dbh_t *dbh,const zend_string *name){pw_pdo_db *db=dbh->driver_data;zval *value=pw_field(&db->response,"insert_id");return value?zval_get_string(value):zend_string_init("0",1,0);}
static void pw_pdo_fetch_error(pdo_dbh_t *dbh,pdo_stmt_t *stmt,zval *info){pw_pdo_db *db=dbh->driver_data;add_next_index_long(info,pw_db_long(&db->response,"errno"));add_next_index_string(info,pw_db_text(&db->response,"error",""));}
static int pw_pdo_attr(pdo_dbh_t *dbh,zend_long attr,zval *out){pw_pdo_db *db=dbh->driver_data;switch(attr){case PDO_ATTR_SERVER_VERSION:ZVAL_STRING(out,db->postgres?"16.4":"8.0.38");return 1;case PDO_ATTR_CLIENT_VERSION:ZVAL_STRING(out,db->postgres?"16.4":"mysqlnd 8.3.30");return 1;case PDO_ATTR_CONNECTION_STATUS:ZVAL_STRING(out,"Localhost via UNIX socket");return 1;case PDO_ATTR_EMULATE_PREPARES:ZVAL_BOOL(out,!db->postgres);return 1;default:return 0;}}
static bool pw_pdo_set_attr(pdo_dbh_t *dbh,zend_long attr,zval *value){return attr==PDO_ATTR_EMULATE_PREPARES && zend_is_true(value)==!((pw_pdo_db*)dbh->driver_data)->postgres;}
static const struct pdo_dbh_methods pw_pdo_methods={.closer=pw_pdo_close,.preparer=pw_pdo_prepare,.doer=pw_pdo_exec,.quoter=pw_pdo_quote,.begin=pw_pdo_begin,.commit=pw_pdo_commit,.rollback=pw_pdo_rollback,.set_attribute=pw_pdo_set_attr,.last_id=pw_pdo_last_id,.fetch_err=pw_pdo_fetch_error,.get_attribute=pw_pdo_attr};
static int pw_pdo_factory(pdo_dbh_t *dbh,zval *options,bool postgres){
    zval *emulate=options && Z_TYPE_P(options)==IS_ARRAY?zend_hash_index_find(Z_ARRVAL_P(options),PDO_ATTR_EMULATE_PREPARES):NULL;
    if(emulate && zend_is_true(emulate)==postgres){pdo_raise_impl_error(dbh,NULL,"IM001","Requested prepare mode is unavailable");return 0;}
    if(dbh->is_persistent){pdo_raise_impl_error(dbh,NULL,"IM001","Persistent connections are unavailable");return 0;}
    pw_pdo_db *db=ecalloc(1,sizeof(*db));db->postgres=postgres;array_init(&db->response);dbh->driver_data=db;dbh->methods=&pw_pdo_methods;dbh->alloc_own_columns=1;dbh->max_escaped_char_length=2;
    struct pdo_data_src_parser vars[]={{"dbname","wordpress",0}};php_pdo_parse_data_source(dbh->data_source,dbh->data_source_len,vars,1);
    zval response;bool ok=pw_db_connect(&db->connection,db->postgres,vars[0].optval,&response);if(!ok)pw_pdo_error(dbh,NULL,&response);zval_ptr_dtor(&response);if(vars[0].freeme)efree(vars[0].optval);return ok;
}
static int pw_pdo_mysql_factory(pdo_dbh_t *dbh,zval *options){return pw_pdo_factory(dbh,options,false);}
static const pdo_driver_t pw_pdo_mysql_driver={PDO_DRIVER_HEADER(mysql),pw_pdo_mysql_factory};
static int pw_pdo_pgsql_factory(pdo_dbh_t *dbh,zval *options){return pw_pdo_factory(dbh,options,true);}
static const pdo_driver_t pw_pdo_pgsql_driver={PDO_DRIVER_HEADER(pgsql),pw_pdo_pgsql_factory};

static const zend_function_entry pw_mysqli_functions[]={
    ZEND_FENTRY(mysqli_connect,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_init,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_real_connect,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_close,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_query,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_real_query,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_prepare,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_execute_query,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_store_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_use_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_more_results,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_next_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_options,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_set_opt,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_select_db,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_real_escape_string,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_escape_string,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_set_charset,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_character_set_name,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_server_info,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_server_version,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_client_info,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_client_version,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_host_info,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_get_proto_info,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_report,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_connect_errno,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_connect_error,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_errno,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_error,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_sqlstate,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_affected_rows,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_insert_id,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_field_count,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_ping,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_begin_transaction,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_commit,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_rollback,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_assoc,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_row,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_array,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_object,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_all,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_free_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_field,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_fields,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_fetch_field_direct,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_num_rows,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_num_fields,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_data_seek,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_execute,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_get_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_store_result,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_close,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_fetch,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_affected_rows,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_insert_id,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_errno,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_error,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_sqlstate,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_num_rows,zif_pw_mysqli_call,pw_db_args,0)
    ZEND_FENTRY(mysqli_stmt_bind_param,zif_pw_mysqli_call,pw_db_bind_args,0)
    ZEND_FENTRY(mysqli_stmt_bind_result,zif_pw_mysqli_call,pw_db_bind_result_args,0)
    ZEND_FE_END
};
static const zend_function_entry pw_mysqli_methods[]={
    ZEND_RAW_FENTRY("__construct",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("real_connect",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("close",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("query",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("real_query",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("prepare",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("execute_query",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("store_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("use_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("more_results",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("next_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("options",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("select_db",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("real_escape_string",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("escape_string",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("set_charset",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("character_set_name",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("ping",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("begin_transaction",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("commit",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("rollback",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_FE_END
};
static const zend_function_entry pw_result_methods[]={
    ZEND_RAW_FENTRY("fetch_assoc",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_row",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_array",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_object",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_all",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("free",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("free_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_field",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_fields",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch_field_direct",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("data_seek",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_FE_END
};
static const zend_function_entry pw_stmt_methods[]={
    ZEND_RAW_FENTRY("execute",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("get_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("store_result",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("close",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("fetch",zif_pw_mysqli_call,pw_db_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("bind_param",zif_pw_mysqli_call,pw_db_bind_method_args,ZEND_ACC_PUBLIC)
    ZEND_RAW_FENTRY("bind_result",zif_pw_mysqli_call,pw_db_bind_result_method_args,ZEND_ACC_PUBLIC)
    ZEND_FE_END
};
static const zend_function_entry pw_pg_functions[]={
    ZEND_FENTRY(pg_connect,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_pconnect,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_close,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_connection_status,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_ping,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_version,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_last_error,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_query,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_query_params,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_prepare,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_execute,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_escape_string,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_escape_literal,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_escape_identifier,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_fetch_assoc,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_fetch_row,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_fetch_array,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_fetch_object,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_fetch_result,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_num_rows,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_num_fields,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_affected_rows,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_result_status,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_field_name,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_field_type,zif_pw_pg_call,pw_db_args,0)
    ZEND_FENTRY(pg_free_result,zif_pw_pg_call,pw_db_args,0)
    ZEND_FE_END
};

static zend_class_entry *pw_db_class(const char *name,const zend_function_entry *methods) {
    zend_class_entry ce;INIT_CLASS_ENTRY_EX(ce,name,strlen(name),methods);
    zend_class_entry *result=zend_register_internal_class(&ce);
    result->create_object=pw_db_create;result->ce_flags|=ZEND_ACC_NOT_SERIALIZABLE;
    return result;
}
static PHP_MINIT_FUNCTION(pw_mysqli) {
    memcpy(&pw_db_handlers,zend_get_std_object_handlers(),sizeof(pw_db_handlers));
    pw_db_handlers.offset=XtOffsetOf(pw_db_object,std);pw_db_handlers.free_obj=pw_db_free;
    pw_db_handlers.get_gc=pw_db_gc;pw_db_handlers.clone_obj=NULL;
    pw_mysqli_ce=pw_db_class("mysqli",pw_mysqli_methods);
    pw_result_ce=pw_db_class("mysqli_result",pw_result_methods);
    pw_stmt_ce=pw_db_class("mysqli_stmt",pw_stmt_methods);
    zend_class_entry ce;INIT_CLASS_ENTRY(ce,"mysqli_sql_exception",NULL);
    pw_sql_exception_ce=zend_register_internal_class_ex(&ce,spl_ce_RuntimeException);
    const char *numbers[]={"affected_rows","insert_id","errno","connect_errno","field_count","server_version","protocol_version","thread_id","num_rows",NULL};
    const char *strings[]={"error","connect_error","sqlstate","server_info","host_info",NULL};
    zend_class_entry *classes[]={pw_mysqli_ce,pw_result_ce,pw_stmt_ce};
    for(unsigned c=0;c<3;c++) {
        for(unsigned i=0;numbers[i];i++)zend_declare_property_long(classes[c],numbers[i],strlen(numbers[i]),0,ZEND_ACC_PUBLIC);
        for(unsigned i=0;strings[i];i++)zend_declare_property_string(classes[c],strings[i],strlen(strings[i]),"",ZEND_ACC_PUBLIC);
    }
    REGISTER_LONG_CONSTANT("MYSQLI_ASSOC",1,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_NUM",2,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_BOTH",3,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("MYSQLI_REPORT_OFF",0,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_REPORT_ERROR",1,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_REPORT_STRICT",2,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_REPORT_ALL",255,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("MYSQLI_STORE_RESULT",0,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_USE_RESULT",1,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("MYSQLI_OPT_CONNECT_TIMEOUT",0,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_SET_CHARSET_NAME",7,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("MYSQLI_CLIENT_FOUND_ROWS",2,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("MYSQLI_CLIENT_SSL",2048,CONST_PERSISTENT);
    return SUCCESS;
}
static PHP_RINIT_FUNCTION(pw_mysqli) { pw_mysqli_report=3;pw_connect_errno=0;pw_connect_error=NULL;return SUCCESS; }
static PHP_RSHUTDOWN_FUNCTION(pw_mysqli) { if(pw_connect_error)zend_string_release(pw_connect_error);pw_connect_error=NULL;return SUCCESS; }
static PHP_MINIT_FUNCTION(pw_pgsql) {
    pw_pg_connection_ce=pw_db_class("PgSql\\Connection",NULL);pw_pg_result_ce=pw_db_class("PgSql\\Result",NULL);
    REGISTER_LONG_CONSTANT("PGSQL_ASSOC",1,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("PGSQL_NUM",2,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("PGSQL_BOTH",3,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("PGSQL_CONNECTION_OK",0,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("PGSQL_CONNECTION_BAD",1,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("PGSQL_COMMAND_OK",1,CONST_PERSISTENT);REGISTER_LONG_CONSTANT("PGSQL_TUPLES_OK",2,CONST_PERSISTENT);
    REGISTER_LONG_CONSTANT("PGSQL_CONNECT_FORCE_NEW",2,CONST_PERSISTENT);return SUCCESS;
}
static PHP_MINIT_FUNCTION(pw_pdo_mysql) { return php_pdo_register_driver(&pw_pdo_mysql_driver); }
static PHP_MSHUTDOWN_FUNCTION(pw_pdo_mysql) { php_pdo_unregister_driver(&pw_pdo_mysql_driver);return SUCCESS; }
static PHP_MINIT_FUNCTION(pw_pdo_pgsql) { return php_pdo_register_driver(&pw_pdo_pgsql_driver); }
static PHP_MSHUTDOWN_FUNCTION(pw_pdo_pgsql) { php_pdo_unregister_driver(&pw_pdo_pgsql_driver);return SUCCESS; }
static zend_module_entry pw_mysqli_module={STANDARD_MODULE_HEADER,"mysqli",pw_mysqli_functions,PHP_MINIT(pw_mysqli),NULL,PHP_RINIT(pw_mysqli),PHP_RSHUTDOWN(pw_mysqli),NULL,PHP_VERSION,STANDARD_MODULE_PROPERTIES};
static zend_module_entry pw_pgsql_module={STANDARD_MODULE_HEADER,"pgsql",pw_pg_functions,PHP_MINIT(pw_pgsql),NULL,NULL,NULL,NULL,PHP_VERSION,STANDARD_MODULE_PROPERTIES};
static zend_module_entry pw_pdo_mysql_module={STANDARD_MODULE_HEADER,"pdo_mysql",NULL,PHP_MINIT(pw_pdo_mysql),PHP_MSHUTDOWN(pw_pdo_mysql),NULL,NULL,NULL,PHP_VERSION,STANDARD_MODULE_PROPERTIES};
static zend_module_entry pw_pdo_pgsql_module={STANDARD_MODULE_HEADER,"pdo_pgsql",NULL,PHP_MINIT(pw_pdo_pgsql),PHP_MSHUTDOWN(pw_pdo_pgsql),NULL,NULL,NULL,PHP_VERSION,STANDARD_MODULE_PROPERTIES};
static void pw_database_startup(void) {
    zend_startup_module(&pw_mysqli_module);zend_startup_module(&pw_pgsql_module);
    zend_startup_module(&pw_pdo_mysql_module);zend_startup_module(&pw_pdo_pgsql_module);
}
