/* Request-scoped provenance by Zend string identity, never substring equality.
 * Holding a reference prevents address reuse and forces copy-on-write. Bounds
 * cap retained input/derived strings at 4 MiB, 4096 identities, 64 ranges each.
 * On exhaustion propagation stops; incomplete evidence cannot confirm a sink. */
static HashTable pw_provenance;
static size_t pw_provenance_bytes;
static bool pw_provenance_limited;
static zval *pw_provenance_record(zend_string *string) {
    return zend_hash_index_find(&pw_provenance,(zend_ulong)(uintptr_t)string);
}
static zval *pw_provenance_create(zend_string *string) {
    zval *existing=pw_provenance_record(string);
    if(existing)return existing;
    if(ZSTR_LEN(string)>4194304-pw_provenance_bytes || zend_hash_num_elements(&pw_provenance)>=4096){pw_provenance_limited=true;return NULL;}
    zval record,ranges;array_init(&record);array_init(&ranges);
    add_assoc_str(&record,"string",zend_string_copy(string));add_assoc_zval(&record,"ranges",&ranges);
    pw_provenance_bytes+=ZSTR_LEN(string);
    return zend_hash_index_update(&pw_provenance,(zend_ulong)(uintptr_t)string,&record);
}
static void pw_string_provenance(zval *out,zend_string *string) {
    zval *record=pw_active?pw_provenance_record(string):NULL;
    zval *ranges=pw_field(record,"ranges");
    if(ranges)ZVAL_DUP(out,ranges);else array_init(out);
}
/* Called from the engine while the result is live. The source may already have
 * been released if untracked; it is used only as an identity lookup key. */
void php_wasm_provenance_merge(zend_string *out,zend_string *source,size_t offset) {
    if(!pw_active || pw_in_hook || out==source)return;
    zval *record=pw_provenance_record(source),*ranges=pw_field(record,"ranges");
    if(!ranges || !zend_hash_num_elements(Z_ARRVAL_P(ranges)))return;
    zval copied;ZVAL_DUP(&copied,ranges);
    zval *target=pw_provenance_create(out),*dest=pw_field(target,"ranges");
    if(dest){
        if(zend_hash_num_elements(Z_ARRVAL_P(dest))+zend_hash_num_elements(Z_ARRVAL(copied))>64){zend_hash_clean(Z_ARRVAL_P(dest));pw_provenance_limited=true;}
        else {zval *range;ZEND_HASH_FOREACH_VAL(Z_ARRVAL(copied),range){zval value;ZVAL_DUP(&value,range);zval *start=pw_field(&value,"start");add_assoc_long(&value,"start",(start?Z_LVAL_P(start):0)+offset);add_next_index_zval(dest,&value);}ZEND_HASH_FOREACH_END();}
    }
    zval_ptr_dtor(&copied);
}
static void pw_provenance_input(zval *input,const char *source,unsigned depth) {
    if(depth>5 || pw_provenance_bytes>=4194304)return;ZVAL_DEREF(input);
    if(Z_TYPE_P(input)==IS_STRING && Z_STRLEN_P(input)) {
        /* Never mark a shared interned string as request-originated. */
        zend_string *copy=zend_string_init(Z_STRVAL_P(input),Z_STRLEN_P(input),0);
        zval_ptr_dtor(input);ZVAL_STR(input,copy);
        zval *record=pw_provenance_create(copy),*ranges=pw_field(record,"ranges");
        if(ranges){zval range;array_init(&range);add_assoc_string(&range,"origin","request");add_assoc_string(&range,"source",(char*)source);add_assoc_long(&range,"start",0);add_assoc_long(&range,"length",ZSTR_LEN(copy));add_next_index_zval(ranges,&range);}
    } else if(Z_TYPE_P(input)==IS_ARRAY || (Z_TYPE_P(input)==IS_OBJECT && Z_OBJCE_P(input)==zend_standard_class_def)) {
        zend_string *key;zend_ulong index;zval *value;unsigned count=0;
        HashTable *items=Z_TYPE_P(input)==IS_ARRAY?Z_ARRVAL_P(input):Z_OBJPROP_P(input);
        ZEND_HASH_FOREACH_KEY_VAL(items,index,key,value){
            if(++count>512){pw_provenance_limited=true;break;}
            zend_string *path=key?strpprintf(0,"%s.%.*s",source,(int)(ZSTR_LEN(key)>128?128:ZSTR_LEN(key)),ZSTR_VAL(key)):strpprintf(0,"%s.%lu",source,(unsigned long)index);
            pw_provenance_input(value,ZSTR_VAL(path),depth+1);zend_string_release(path);
        }ZEND_HASH_FOREACH_END();
    }
}
static void pw_provenance_transform(zend_execute_data *frame,zval *retval) {
    if(!pw_active || pw_in_hook || !retval || !frame->func || frame->func->type!=ZEND_INTERNAL_FUNCTION || !ZEND_CALL_NUM_ARGS(frame))return;
    const char *name=ZSTR_VAL(frame->func->common.function_name);
    zval *input=ZEND_CALL_ARG(frame,1);ZVAL_DEREF(input);
    if(!strcmp(name,"file_get_contents") && Z_TYPE_P(input)==IS_STRING && !strcmp(Z_STRVAL_P(input),"php://input") && Z_TYPE_P(retval)==IS_STRING){pw_provenance_input(retval,"body",0);return;}
    if(!strcmp(name,"json_decode") && Z_TYPE_P(input)==IS_STRING){
        zval *record=pw_provenance_record(Z_STR_P(input)),*ranges=pw_field(record,"ranges");
        if(ranges && zend_hash_num_elements(Z_ARRVAL_P(ranges))==1){zval *range=zend_hash_index_find(Z_ARRVAL_P(ranges),0),*start=pw_field(range,"start"),*length=pw_field(range,"length");if(start && length && Z_LVAL_P(start)==0 && Z_LVAL_P(length)==Z_STRLEN_P(input)){zval *origin=pw_field(range,"source");zend_string *path=strpprintf(0,"%s.json",origin?Z_STRVAL_P(origin):"request");pw_provenance_input(retval,ZSTR_VAL(path),0);zend_string_release(path);}}
        return;
    }
    if(Z_TYPE_P(retval)!=IS_STRING)return;
    /* These functions derive their complete output from argument zero. Range
     * offsets are rebuilt; mixed-source formatting is deliberately unsupported. */
    const char *names[]={"stripslashes","addslashes","trim","ltrim","rtrim","strtolower","strtoupper","urldecode","rawurldecode","base64_decode","substr",NULL};
    bool supported=false;for(unsigned i=0;names[i];i++)if(!strcmp(name,names[i])){supported=true;break;}
    if(!supported)return;
    zval *arg=ZEND_CALL_ARG(frame,1);ZVAL_DEREF(arg);if(Z_TYPE_P(arg)!=IS_STRING || Z_STR_P(arg)==Z_STR_P(retval))return;
    zval *record=pw_provenance_record(Z_STR_P(arg)),*ranges=pw_field(record,"ranges");
    if(!ranges || !zend_hash_num_elements(Z_ARRVAL_P(ranges)))return;
    /* Only a wholly request-derived input permits a wholly derived output. */
    if(zend_hash_num_elements(Z_ARRVAL_P(ranges))!=1)return;
    zval *range=zend_hash_index_find(Z_ARRVAL_P(ranges),0),*start=pw_field(range,"start"),*length=pw_field(range,"length");
    if(!start || !length || Z_LVAL_P(start)!=0 || Z_LVAL_P(length)!=Z_STRLEN_P(arg))return;
    zval copy;ZVAL_DUP(&copy,range);add_assoc_long(&copy,"length",Z_STRLEN_P(retval));add_assoc_string(&copy,"transform",(char*)name);
    zval *target=pw_provenance_create(Z_STR_P(retval)),*dest=pw_field(target,"ranges");
    if(dest && !zend_hash_num_elements(Z_ARRVAL_P(dest)))add_next_index_zval(dest,&copy);else zval_ptr_dtor(&copy);
}
