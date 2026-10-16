/*
 * Host-controlled instrumentation for the PHP 8.3 WASI build.
 * Included by main/main.c only in an explicitly instrumented build.
 * Copyright (c) php-wasm contributors. MIT license (see repository LICENSE).
 */
#include "Zend/zend_observer.h"
#include "Zend/zend_exceptions.h"
#include "Zend/zend_closures.h"
#include "Zend/zend_execute.h"
#include "Zend/zend_smart_str.h"
#include "ext/json/php_json.h"
#include <stdint.h>

#define PW_CONFIG_CAP 262144
#define PW_EVENT_CAP 65536
#define PW_VALUE_BYTES 16384
#define PW_VALUE_ITEMS 32
#define PW_VALUE_DEPTH 4
#define PW_VALUE_NODES 128
#define PW_MAX_EVENTS 512
#define PW_MAX_HOOKS 128

__attribute__((import_module("php_wasm_runtime_v1"), import_name("config")))
extern int32_t pw_host_config(char *buffer, int32_t capacity);
__attribute__((import_module("php_wasm_runtime_v1"), import_name("emit")))
extern int32_t pw_host_emit(const char *buffer, int32_t length);

static bool pw_active, pw_in_hook, pw_frames_initialized;
static uint32_t pw_count, pw_omitted, pw_transport_errors, pw_call_id, pw_compile_id, pw_limit;
static uint32_t pw_application_count, pw_security_count;
static zval pw_config;
static HashTable pw_frames, pw_compilations;
static zend_op_array *(*pw_compile_file_previous)(zend_file_handle *, int);
static zend_op_array *(*pw_compile_string_previous)(zend_string *, const char *, zend_compile_position);

/* Restore the ordinary PHP entry point without claiming a permission change.
 * Register under standard, before disable_functions is applied. */
ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(pw_arginfo_chmod, 0, 2, _IS_BOOL, 0)
    ZEND_ARG_TYPE_INFO(0, filename, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, permissions, IS_LONG, 0)
ZEND_END_ARG_INFO()
static PHP_FUNCTION(pw_chmod)
{
    char *filename;
    size_t length;
    zend_long permissions;
    ZEND_PARSE_PARAMETERS_START(2, 2)
        Z_PARAM_PATH(filename, length)
        Z_PARAM_LONG(permissions)
    ZEND_PARSE_PARAMETERS_END();
    RETURN_FALSE;
}
static const zend_function_entry pw_compat_functions[] = {
    ZEND_FENTRY(chmod, zif_pw_chmod, pw_arginfo_chmod, 0)
    ZEND_FE_END
};

static zval *pw_field(zval *value, const char *name)
{
    return value && Z_TYPE_P(value) == IS_ARRAY
        ? zend_hash_str_find(Z_ARRVAL_P(value), name, strlen(name)) : NULL;
}

#include "php_wasm_provenance.c"

/* Snapshot without invoking __toString, JsonSerializable, magic properties,
 * iterators or other application code. Objects/resources are identities only. */
static void pw_snapshot(zval *out, zval *value, unsigned depth, unsigned *budget)
{
    if (!value) { ZVAL_NULL(out); return; }
    ZVAL_DEREF(value);
    if (!*budget || depth > PW_VALUE_DEPTH) {
        array_init(out);
        add_assoc_bool(out, "truncated", true);
        return;
    }
    --*budget;
    switch (Z_TYPE_P(value)) {
        case IS_UNDEF: case IS_NULL: ZVAL_NULL(out); break;
        case IS_TRUE: case IS_FALSE: case IS_LONG: case IS_DOUBLE:
            ZVAL_COPY(out, value); break;
        case IS_STRING:
            if (Z_STRLEN_P(value) <= PW_VALUE_BYTES) {
                ZVAL_COPY(out, value);
            } else {
                array_init(out);
                add_assoc_stringl(out, "prefix", Z_STRVAL_P(value), PW_VALUE_BYTES);
                add_assoc_long(out, "bytes", Z_STRLEN_P(value));
                add_assoc_bool(out, "truncated", true);
            }
            break;
        case IS_ARRAY: {
            zval items, *item;
            zend_string *key;
            zend_ulong index;
            unsigned count = 0;
            array_init(&items);
            ZEND_HASH_FOREACH_KEY_VAL(Z_ARRVAL_P(value), index, key, item) {
                if (count++ >= PW_VALUE_ITEMS || !*budget) break;
                zval copy;
                pw_snapshot(&copy, item, depth + 1, budget);
                if (key) zend_hash_update(Z_ARRVAL(items), key, &copy);
                else zend_hash_index_update(Z_ARRVAL(items), index, &copy);
            } ZEND_HASH_FOREACH_END();
            array_init(out);
            add_assoc_string(out, "type", "array");
            add_assoc_long(out, "count", zend_hash_num_elements(Z_ARRVAL_P(value)));
            add_assoc_bool(out, "truncated", zend_hash_num_elements(Z_ARRVAL(items)) < zend_hash_num_elements(Z_ARRVAL_P(value)));
            add_assoc_zval(out, "items", &items);
            break;
        }
        case IS_OBJECT:
            array_init(out);
            add_assoc_string(out, "type", "object");
            add_assoc_str(out, "class", zend_string_copy(Z_OBJCE_P(value)->name));
            add_assoc_long(out, "id", Z_OBJ_HANDLE_P(value));
            break;
        case IS_RESOURCE:
            array_init(out);
            add_assoc_string(out, "type", "resource");
            add_assoc_long(out, "id", Z_RES_HANDLE_P(value));
            break;
        default: ZVAL_NULL(out); break;
    }
}

static void pw_add_snapshot(zval *event, const char *key, zval *value)
{
    zval copy;
    unsigned budget = PW_VALUE_NODES;
    pw_snapshot(&copy, value, 0, &budget);
    add_assoc_zval(event, key, &copy);
}

static void pw_location(zval *event, zend_execute_data *frame)
{
    unsigned depth = 0;
    while (frame && depth++ < 64) {
        if (frame->func && ZEND_USER_CODE(frame->func->type)) {
            if (frame->func->op_array.filename)
                add_assoc_str(event, "file", zend_string_copy(frame->func->op_array.filename));
            if (frame->opline) add_assoc_long(event, "line", frame->opline->lineno);
            return;
        }
        frame = frame->prev_execute_data;
    }
}

static void pw_event(zval *event, const char *kind)
{
    array_init(event);
    add_assoc_long(event, "version", 1);
    add_assoc_string(event, "kind", (char *)kind);
}

static void pw_emit(zval *event, bool force)
{
    if (!pw_active) return;
    zval *kind=pw_field(event,"kind"),*derived=pw_field(event,"request_derived");
    bool application=kind && !strcmp(Z_STRVAL_P(kind),"application.observation");
    bool security=(kind && !strcmp(Z_STRVAL_P(kind),"security.evidence")) || (derived && zend_is_true(derived));
    uint32_t *count=application?&pw_application_count:(security?&pw_security_count:&pw_count);
    uint32_t limit=application?64:(security?128:pw_limit);
    if (!force && *count >= limit) { ++pw_omitted; return; }
    smart_str json = {0};
    /* Encoding must not alter json_last_error() in the observed application. */
    int saved_error = JSON_G(error_code);
    zend_result result = php_json_encode(&json, event, PHP_JSON_INVALID_UTF8_SUBSTITUTE | PHP_JSON_UNESCAPED_SLASHES);
    JSON_G(error_code) = saved_error;
    if (result == SUCCESS && json.s && ZSTR_LEN(json.s) <= PW_EVENT_CAP) {
        if (!force) ++*count;
        if (pw_host_emit(ZSTR_VAL(json.s), ZSTR_LEN(json.s)) != 0) ++pw_transport_errors;
    } else {
        ++pw_omitted;
    }
    smart_str_free(&json);
}

#include "php_wasm_database.c"

static const char *pw_sink(const char *name)
{
    static const char *files[] = {"fopen", "file_get_contents", "file_put_contents", "readfile", "file", "fwrite", "unlink", "rename", "copy", "mkdir", "rmdir", "chmod", "move_uploaded_file", NULL};
    static const char *process[] = {"exec", "system", "shell_exec", "passthru", "popen", "proc_open", "pcntl_exec", NULL};
    static const char *network[] = {"fsockopen", "pfsockopen", "stream_socket_client", "curl_exec", "mail", NULL};
    static const char *sql[] = {"pdo::query", "pdo::exec", "pdo::prepare", "pdostatement::execute", "pdostatement::bindvalue", "pdostatement::bindparam", "sqlite3::query", "sqlite3::exec", "sqlite3::prepare", "sqlite3stmt::execute", "sqlite3stmt::bindvalue", "sqlite3stmt::bindparam", "mysqli_query", "mysqli_real_query", "mysqli_multi_query", "mysqli_prepare", "mysqli_stmt_execute", "mysqli::query", "mysqli::real_query", "mysqli::multi_query", "mysqli::prepare", "mysqli_stmt::execute", "pg_query", "pg_query_params", "pg_prepare", "pg_execute", NULL};
    const char **groups[] = {files, process, network, sql};
    const char *kinds[] = {"file", "process", "network", "database"};
    for (unsigned group = 0; group < 4; ++group)
        for (const char **p = groups[group]; *p; ++p)
            if (!strcmp(name, *p)) return kinds[group];
    if (!strcmp(name, "unserialize")) return "deserialization";
    return NULL;
}

static zend_string *pw_name(zend_execute_data *frame)
{
    zend_function *fn = frame->func;
    if (!fn || !fn->common.function_name) return NULL;
    zend_string *name;
    if (fn->common.scope)
        name = strpprintf(0, "%s::%s", ZSTR_VAL(fn->common.scope->name), ZSTR_VAL(fn->common.function_name));
    else name = zend_string_copy(fn->common.function_name);
    zend_string *lower = zend_string_tolower(name);
    zend_string_release(name);
    return lower;
}

static zval *pw_hook(zend_string *name)
{
    zval *hooks = pw_field(&pw_config, "hooks");
    return hooks && Z_TYPE_P(hooks) == IS_ARRAY
        ? zend_hash_find(Z_ARRVAL_P(hooks), name) : NULL;
}

static bool pw_selected(zend_string *name)
{
    if (pw_sink(ZSTR_VAL(name)) || pw_hook(name)) return true;
    zval *observe = pw_field(&pw_config, "observe"), *entry;
    if (observe && Z_TYPE_P(observe) == IS_ARRAY) {
        ZEND_HASH_FOREACH_VAL(Z_ARRVAL_P(observe), entry) {
            if (Z_TYPE_P(entry) == IS_STRING && zend_string_equals(Z_STR_P(entry), name)) return true;
        } ZEND_HASH_FOREACH_END();
    }
    return false;
}

/* Scripts run as closures: their local variables do not overwrite application
 * locals. Arguments/result are copies; contained objects remain live objects.
 * Return {arguments: [...], result: ..., emit: ...} to request explicit changes.
 * Exceptions intentionally propagate; an invalid operator script is not silently
 * accepted as a compatibility success. Scripts cannot bypass WASI capabilities. */
static void pw_script(zval *script, zend_execute_data *frame, zval *retval, zval *event, bool before)
{
    if (!script || Z_TYPE_P(script) != IS_STRING || !Z_STRLEN_P(script) || EG(exception)) return;
    zval callback, result, params[4];
    ZVAL_UNDEF(&callback);
    ZVAL_UNDEF(&result);
    array_init(&params[0]);
    if (frame) {
        for (uint32_t i = 0; i < ZEND_CALL_NUM_ARGS(frame); ++i) {
            zval value;
            ZVAL_COPY_DEREF(&value, ZEND_CALL_ARG(frame, i + 1));
            add_next_index_zval(&params[0], &value);
        }
    }
    if (retval) ZVAL_COPY_DEREF(&params[1], retval); else ZVAL_NULL(&params[1]);
    if (frame && Z_TYPE(frame->This) == IS_OBJECT) ZVAL_OBJ_COPY(&params[2], Z_OBJ(frame->This));
    else ZVAL_NULL(&params[2]);
    ZVAL_COPY(&params[3], event);
    zend_string *expression = strpprintf(0, "static function (array $arguments, $result, $object, array $event) {\n%s\n}", Z_STRVAL_P(script));
    pw_in_hook = true;
    zend_try {
        if (zend_eval_stringl(ZSTR_VAL(expression), ZSTR_LEN(expression), &callback, "runtime://host-script") == SUCCESS && !EG(exception)) {
            call_user_function(NULL, NULL, &callback, &result, 4, params);
        }
    } zend_catch {
        pw_in_hook = false;
        zend_bailout();
    } zend_end_try();
    zend_string_release(expression);
    if (!Z_ISUNDEF(callback)) zval_ptr_dtor(&callback);
    for (unsigned i = 0; i < 4; ++i) zval_ptr_dtor(&params[i]);
    if (!EG(exception) && Z_TYPE(result) == IS_ARRAY) {
        zval *arguments = pw_field(&result, "arguments");
        if (before && frame && arguments && Z_TYPE_P(arguments) == IS_ARRAY) {
            for (uint32_t i = 0; i < ZEND_CALL_NUM_ARGS(frame); ++i) {
                zval *replacement = zend_hash_index_find(Z_ARRVAL_P(arguments), i);
                if (!replacement) continue;
                zval *argument = ZEND_CALL_ARG(frame, i + 1);
                /* References can be bound to typed properties. Use the engine's
                 * assignment path so operator replacements preserve their type
                 * constraints and ownership, including reference return values. */
                zend_assign_to_variable(argument, replacement, IS_CV, true);
                if (EG(exception)) break;
            }
        }
        zval *replacement = pw_field(&result, "result");
        if (!before && retval && replacement && !EG(exception)) {
            zend_assign_to_variable(retval, replacement, IS_CV, true);
        }
        zval *observation = pw_field(&result, "emit");
        if (observation) {
            zval custom;
            pw_event(&custom, "application.observation");
            pw_add_snapshot(&custom, "data", observation);
            zval *id = pw_field(event, "call_id");
            if (id) add_assoc_long(&custom, "call_id", Z_LVAL_P(id));
            pw_emit(&custom, false);
            zval_ptr_dtor(&custom);
        }
    }
    if (!Z_ISUNDEF(result)) zval_ptr_dtor(&result);
    pw_in_hook = false;
}

static void pw_arguments(zval *event, zend_execute_data *frame)
{
    zval args;
    unsigned budget = PW_VALUE_NODES;
    array_init(&args);
    for (uint32_t i = 0; i < ZEND_CALL_NUM_ARGS(frame) && i < PW_VALUE_ITEMS && budget; ++i) {
        zval value;
        pw_snapshot(&value, ZEND_CALL_ARG(frame, i + 1), 0, &budget);
        add_next_index_zval(&args, &value);
    }
    zval origins;array_init(&origins);bool derived=false;
    for(uint32_t i=0;i<ZEND_CALL_NUM_ARGS(frame) && i<PW_VALUE_ITEMS;i++){
        zval ranges,*arg=ZEND_CALL_ARG(frame,i+1);ZVAL_DEREF(arg);
        if(Z_TYPE_P(arg)==IS_STRING)pw_string_provenance(&ranges,Z_STR_P(arg));else array_init(&ranges);
        if(zend_hash_num_elements(Z_ARRVAL(ranges)))derived=true;
        add_next_index_zval(&origins,&ranges);
    }
    add_assoc_zval(event,"argument_provenance",&origins);
    if(derived)add_assoc_bool(event,"request_derived",true);
    add_assoc_long(event, "argument_count", ZEND_CALL_NUM_ARGS(frame));
    add_assoc_zval(event, "arguments", &args);
    if (Z_TYPE(frame->This) == IS_OBJECT) add_assoc_long(event, "object_id", Z_OBJ_HANDLE(frame->This));
}

static void pw_begin(zend_execute_data *frame)
{
    if (!pw_active || pw_in_hook || !frame->func) return;
    zend_string *name = pw_name(frame);
    bool code = !name && ZEND_USER_CODE(frame->func->type);
    if (!code && (!name || !pw_selected(name))) {
        if (name) zend_string_release(name);
        return;
    }
    zval event;
    pw_event(&event, code ? "code.enter" : "call.begin");
    add_assoc_long(&event, "call_id", ++pw_call_id);
    pw_location(&event, code ? frame : frame->prev_execute_data);
    if (name) {
        add_assoc_str(&event, "function", zend_string_copy(name));
        const char *sink = pw_sink(ZSTR_VAL(name));
        if (sink) add_assoc_string(&event, "sink", (char *)sink);
        add_assoc_bool(&event, "internal", frame->func->type == ZEND_INTERNAL_FUNCTION);
        add_assoc_bool(&event, "operator_hook", pw_hook(name) != NULL);
        pw_arguments(&event, frame);
    } else {
        add_assoc_string(&event, "code_type", frame->func->type == ZEND_EVAL_CODE ? "eval" : "file");
        zval *compilation = zend_hash_index_find(&pw_compilations, (zend_ulong)(uintptr_t)&frame->func->op_array);
        if (compilation) {
            add_assoc_long(&event,"compile_id",pw_db_long(compilation,"compile_id"));
            zval *derived=pw_field(compilation,"request_derived");if(derived)add_assoc_bool(&event,"request_derived",zend_is_true(derived));
        }
    }
    pw_emit(&event, false);
    zval stored;
    ZVAL_COPY(&stored, &event);
    zend_hash_index_update(&pw_frames, (zend_ulong)(uintptr_t)frame, &stored);
    if (name) pw_script(pw_field(pw_hook(name), "before"), frame, NULL, &event, true);
    zval_ptr_dtor(&event);
    if (name) zend_string_release(name);
}

static void pw_end(zend_execute_data *frame, zval *retval)
{
    if (!pw_active || pw_in_hook) return;
    pw_provenance_transform(frame,retval);
    zval *stored = zend_hash_index_find(&pw_frames, (zend_ulong)(uintptr_t)frame);
    if (!stored) return;
    zval event;
    ZVAL_DUP(&event, stored);
    bool code = !pw_field(&event, "function");
    add_assoc_string(&event, "kind", code ? "code.leave" : "call.end");
    add_assoc_string(&event, "completion", EG(exception) ? "exception" : (retval ? "returned" : "unwound"));
    if (EG(exception)) add_assoc_str(&event, "exception", zend_string_copy(EG(exception)->ce->name));
    if (retval) pw_add_snapshot(&event, "result", retval);
    zval *sink = pw_field(&event, "sink");
    if (sink && Z_TYPE_P(sink) == IS_STRING && !strcmp(Z_STRVAL_P(sink), "process"))
        add_assoc_string(&event, "operation_outcome", "unavailable");
    zval *function = pw_field(&event, "function");
    if (function && Z_TYPE_P(function) == IS_STRING && !strcmp(Z_STRVAL_P(function), "chmod"))
        add_assoc_string(&event, "operation_outcome", "unavailable");
    /* Record the original outcome before any operator-provided replacement. */
    pw_emit(&event, false);
    zend_string *name = pw_name(frame);
    if (name) {
        pw_script(pw_field(pw_hook(name), "after"), frame, retval, &event, false);
        zend_string_release(name);
    }
    zend_hash_index_del(&pw_frames, (zend_ulong)(uintptr_t)frame);
    zval_ptr_dtor(&event);
}

static zend_observer_fcall_handlers pw_observer_init(zend_execute_data *frame)
{
    /* Always install: internal-function observer caches survive FastCGI
     * requests, while host hook configuration may change per request. */
    return (zend_observer_fcall_handlers){pw_begin, pw_end};
}

static void pw_error(int type, zend_string *file, uint32_t line, zend_string *message)
{
    if (!pw_active) return;
    zval event, value;
    pw_event(&event, "runtime.error");
    add_assoc_long(&event, "error_type", type);
    add_assoc_long(&event, "line", line);
    add_assoc_bool(&event, "injection", pw_in_hook);
    if (file) add_assoc_str(&event, "file", zend_string_copy(file));
    ZVAL_STR(&value, message);
    pw_add_snapshot(&event, "message", &value);
    pw_emit(&event, false);
    zval_ptr_dtor(&event);
}

static zend_op_array *pw_compile_file(zend_file_handle *file, int type)
{
    if (!pw_active || pw_in_hook) return pw_compile_file_previous(file, type);
    zval event;
    pw_event(&event, "code.compile");
    uint32_t id = ++pw_compile_id;
    add_assoc_long(&event, "compile_id", id);
    add_assoc_string(&event, "code_type", "file");
    add_assoc_long(&event, "include_type", type);
    if (file->filename) {
        add_assoc_str(&event,"target",zend_string_copy(file->filename));
        zval ranges;pw_string_provenance(&ranges,file->filename);
        if(zend_hash_num_elements(Z_ARRVAL(ranges)))add_assoc_bool(&event,"request_derived",true);
        add_assoc_zval(&event,"provenance",&ranges);
    }
    pw_location(&event, EG(current_execute_data));
    pw_emit(&event, false);
    zend_op_array *result = pw_compile_file_previous(file, type);
    if (result) {
        zval value;
        ZVAL_COPY(&value, &event);
        zend_hash_index_update(&pw_compilations, (zend_ulong)(uintptr_t)result, &value);
    }
    add_assoc_string(&event, "kind", "code.compiled");
    add_assoc_bool(&event, "compiled", result != NULL);
    pw_emit(&event, false);
    zval_ptr_dtor(&event);
    return result;
}

static zend_op_array *pw_compile_string(zend_string *source, const char *filename, zend_compile_position position)
{
    if (!pw_active || pw_in_hook) return pw_compile_string_previous(source, filename, position);
    zval event, value;
    pw_event(&event, "code.compile");
    uint32_t id = ++pw_compile_id;
    add_assoc_long(&event, "compile_id", id);
    add_assoc_string(&event, "code_type", "eval");
    if (filename) add_assoc_string(&event, "target", (char *)filename);
    zval ranges;pw_string_provenance(&ranges,source);
    if(zend_hash_num_elements(Z_ARRVAL(ranges)))add_assoc_bool(&event,"request_derived",true);
    add_assoc_zval(&event,"provenance",&ranges);
    ZVAL_STR(&value, source);
    pw_add_snapshot(&event, "source", &value);
    pw_location(&event, EG(current_execute_data));
    pw_emit(&event, false);
    zend_op_array *result = pw_compile_string_previous(source, filename, position);
    if (result) {
        zval value;
        ZVAL_COPY(&value, &event);
        zend_hash_index_update(&pw_compilations, (zend_ulong)(uintptr_t)result, &value);
    }
    add_assoc_string(&event, "kind", "code.compiled");
    add_assoc_bool(&event, "compiled", result != NULL);
    pw_emit(&event, false);
    zval_ptr_dtor(&event);
    return result;
}

static void php_wasm_runtime_startup(void)
{
    ZVAL_UNDEF(&pw_config);
    pw_database_startup();
    if (!zend_hash_str_exists(CG(function_table), ZEND_STRL("chmod"))) {
        zend_module_entry *saved = EG(current_module);
        EG(current_module) = zend_hash_str_find_ptr(&module_registry, ZEND_STRL("standard"));
        zend_register_functions(NULL, pw_compat_functions, NULL, MODULE_PERSISTENT);
        EG(current_module) = saved;
    }
    zend_observer_fcall_register(pw_observer_init);
    zend_observer_error_register(pw_error);
    pw_compile_file_previous = zend_compile_file;
    zend_compile_file = pw_compile_file;
    pw_compile_string_previous = zend_compile_string;
    zend_compile_string = pw_compile_string;
}

static void php_wasm_runtime_activate(void)
{
    pw_active = false;
    pw_in_hook = false;
    pw_count = pw_omitted = pw_transport_errors = pw_call_id = pw_compile_id = 0;
    pw_limit = PW_MAX_EVENTS;
    pw_application_count=pw_security_count=0;
    pw_provenance_bytes=0;pw_provenance_limited=false;
    zend_hash_init(&pw_provenance,32,NULL,ZVAL_PTR_DTOR,0);
    zend_hash_init(&pw_frames, 16, NULL, ZVAL_PTR_DTOR, 0);
    zend_hash_init(&pw_compilations, 16, NULL, ZVAL_PTR_DTOR, 0);
    pw_frames_initialized = true;
    char *buffer = emalloc(PW_CONFIG_CAP + 1);
    int32_t length = pw_host_config(buffer, PW_CONFIG_CAP);
    /* PHP's JSON scanner requires the ordinary zend_string NUL sentinel even
     * when an explicit length is supplied. FastCGI reuses this allocation. */
    if (length >= 0 && length <= PW_CONFIG_CAP) buffer[length] = '\0';
    int saved_error = JSON_G(error_code);
    if (length <= 0 || length > PW_CONFIG_CAP ||
        php_json_decode_ex(&pw_config, buffer, length, PHP_JSON_OBJECT_AS_ARRAY, 16) != SUCCESS ||
        Z_TYPE(pw_config) != IS_ARRAY) {
        efree(buffer);
        JSON_G(error_code) = saved_error;
        const char *diagnostic = "{\"version\":1,\"kind\":\"configuration.error\",\"reason\":\"invalid_json_or_buffer\"}";
        pw_host_emit(diagnostic, strlen(diagnostic));
        zend_error_noreturn(E_ERROR, "Invalid host runtime configuration");
        return;
    }
    JSON_G(error_code) = saved_error;
    efree(buffer);
    zval *version = pw_field(&pw_config, "version");
    if (!version || Z_TYPE_P(version) != IS_LONG || Z_LVAL_P(version) != 1) {
        const char *diagnostic = "{\"version\":1,\"kind\":\"configuration.error\",\"reason\":\"unsupported_version\"}";
        pw_host_emit(diagnostic, strlen(diagnostic));
        zend_error_noreturn(E_ERROR, "Unsupported host runtime configuration version");
    }
    zval *hooks = pw_field(&pw_config, "hooks"), *observe = pw_field(&pw_config, "observe");
    if ((hooks && (Z_TYPE_P(hooks) != IS_ARRAY || zend_hash_num_elements(Z_ARRVAL_P(hooks)) > PW_MAX_HOOKS)) ||
        (observe && (Z_TYPE_P(observe) != IS_ARRAY || zend_hash_num_elements(Z_ARRVAL_P(observe)) > PW_MAX_HOOKS)))
    {
        const char *diagnostic = "{\"version\":1,\"kind\":\"configuration.error\",\"reason\":\"invalid_hooks\"}";
        pw_host_emit(diagnostic, strlen(diagnostic));
        zend_error_noreturn(E_ERROR, "Invalid host runtime hook list");
    }
    zval *limit = pw_field(&pw_config, "max_events");
    if (limit && Z_TYPE_P(limit) == IS_LONG && Z_LVAL_P(limit) >= 1 && Z_LVAL_P(limit) <= PW_MAX_EVENTS)
        pw_limit = Z_LVAL_P(limit);
    pw_active = true;
    pw_provenance_input(&PG(http_globals)[TRACK_VARS_GET],"get",0);
    pw_provenance_input(&PG(http_globals)[TRACK_VARS_POST],"post",0);
    pw_provenance_input(&PG(http_globals)[TRACK_VARS_COOKIE],"cookie",0);
    zend_is_auto_global_str(ZEND_STRL("_SERVER"));
    if(Z_TYPE(PG(http_globals)[TRACK_VARS_SERVER])==IS_ARRAY){
        zend_string *key;zval *value;
        ZEND_HASH_FOREACH_STR_KEY_VAL(Z_ARRVAL(PG(http_globals)[TRACK_VARS_SERVER]),key,value){
            if(key && (!strncmp(ZSTR_VAL(key),"HTTP_",5) || !strncmp(ZSTR_VAL(key),"PHP_AUTH_",9) || !strcmp(ZSTR_VAL(key),"REQUEST_URI") || !strcmp(ZSTR_VAL(key),"QUERY_STRING"))) {
                zend_string *source=strpprintf(0,"server.%s",ZSTR_VAL(key));pw_provenance_input(value,ZSTR_VAL(source),0);zend_string_release(source);
            }
        }ZEND_HASH_FOREACH_END();
    }
    zval event;
    pw_event(&event, "request.start");
    if (SG(request_info).request_method) add_assoc_string(&event, "method", (char *)SG(request_info).request_method);
    if (SG(request_info).request_uri) add_assoc_string(&event, "uri", SG(request_info).request_uri);
    pw_add_snapshot(&event, "get", &PG(http_globals)[TRACK_VARS_GET]);
    pw_add_snapshot(&event, "post", &PG(http_globals)[TRACK_VARS_POST]);
    pw_emit(&event, false);
    pw_script(pw_field(&pw_config, "bootstrap"), NULL, NULL, &event, true);
    zval_ptr_dtor(&event);
}

static void php_wasm_runtime_deactivate(void)
{
    if (pw_active) {
        zval event;
        pw_event(&event, "request.end");
        add_assoc_long(&event, "events", pw_count);
        add_assoc_long(&event, "omitted", pw_omitted);
        add_assoc_long(&event,"application_events",pw_application_count);
        add_assoc_long(&event,"security_events",pw_security_count);
        add_assoc_bool(&event,"provenance_limited",pw_provenance_limited);
        add_assoc_long(&event, "transport_errors", pw_transport_errors);
        add_assoc_long(&event, "open_calls", zend_hash_num_elements(&pw_frames));
        add_assoc_long(&event, "status", SG(sapi_headers).http_response_code);
        pw_emit(&event, true);
        zval_ptr_dtor(&event);
    }
    pw_active = false;
    pw_in_hook = false;
    if (pw_frames_initialized) {
        zend_hash_destroy(&pw_frames);
        zend_hash_destroy(&pw_compilations);
        zend_hash_destroy(&pw_provenance);
    }
    pw_frames_initialized = false;
    if (!Z_ISUNDEF(pw_config)) zval_ptr_dtor(&pw_config);
    ZVAL_UNDEF(&pw_config);
}
