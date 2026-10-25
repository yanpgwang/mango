/* Independently authored research wrapper, not a production evaluator. */
#define _POSIX_C_SOURCE 200809L
#include "quickjs.h"
#include <stdio.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static struct timespec deadline;
static int allocation_failed;
/* An independently written allocator latches a resource failure outside JS.
   A catch block cannot turn OOM into a new host effect after a failed budget. */
typedef union { max_align_t alignment; size_t bytes; } allocation;
static void *allocate(JSMallocState *s, size_t size) {
    if (size > SIZE_MAX - sizeof(allocation) || size + sizeof(allocation) > s->malloc_limit ||
        s->malloc_size > s->malloc_limit - size - sizeof(allocation)) {
        allocation_failed = 1; return NULL;
    }
    allocation *p = malloc(size + sizeof(allocation));
    if (!p) { allocation_failed = 1; return NULL; }
    p->bytes = size;
    s->malloc_size += size + sizeof(allocation); s->malloc_count++;
    return p + 1;
}
static void release(JSMallocState *s, void *ptr) {
    if (!ptr) return;
    allocation *p = (allocation *)ptr - 1;
    s->malloc_size -= p->bytes + sizeof(allocation); s->malloc_count--;
    free(p);
}
static void *resize(JSMallocState *s, void *ptr, size_t size) {
    if (!ptr) return size ? allocate(s, size) : NULL;
    if (!size) { release(s, ptr); return NULL; }
    allocation *p = (allocation *)ptr - 1;
    size_t old = p->bytes;
    if (size > SIZE_MAX - sizeof(allocation) || size + sizeof(allocation) > s->malloc_limit ||
        s->malloc_size - old - sizeof(allocation) > s->malloc_limit - size - sizeof(allocation)) {
        allocation_failed = 1; return NULL;
    }
    p = realloc(p, size + sizeof(allocation));
    if (!p) { allocation_failed = 1; return NULL; }
    p->bytes = size;
    s->malloc_size = s->malloc_size - old + size;
    return p + 1;
}
static size_t usable(const void *ptr) { return ptr ? ((const allocation *)ptr - 1)->bytes : 0; }
static int expired(JSRuntime *rt, void *opaque) {
    (void)rt; (void)opaque;
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    return allocation_failed || now.tv_sec > deadline.tv_sec || (now.tv_sec == deadline.tv_sec && now.tv_nsec > deadline.tv_nsec);
}
static void quantum(void) {
    clock_gettime(CLOCK_MONOTONIC, &deadline);
    deadline.tv_nsec += 500000000;
    if (deadline.tv_nsec >= 1000000000) { deadline.tv_sec++; deadline.tv_nsec -= 1000000000; }
}
static void fail(void) {
    puts("{\"kind\":\"failure\",\"code\":\"engine_failure\"}");
    fflush(stdout);
    exit(65);
}
static JSValue call(JSContext *ctx, JSValueConst driver, const char *name, int argc, JSValueConst *argv) {
    JSValue method = JS_GetPropertyStr(ctx, driver, name);
    JSValue result = JS_Call(ctx, method, driver, argc, argv);
    JS_FreeValue(ctx, method);
    if (JS_IsException(result)) fail();
    return result;
}

int main(void) {
    FILE *bootstrap = fopen("/realm.js", "rb");
    if (!bootstrap) return 66;
    char realm[16384];
    size_t size = fread(realm, 1, sizeof(realm)-1, bootstrap);
    fclose(bootstrap);
    realm[size] = '\0';
    const JSMallocFunctions allocator = {allocate, release, resize, usable};
    JSRuntime *rt = JS_NewRuntime2(&allocator, NULL);
    if (!rt) return 66;
    JS_SetMemoryLimit(rt, 48 * 1024 * 1024);
    /* OS RLIMIT_STACK owns fatal stack exhaustion in this research profile;
       a catchable engine stack exception is not our containment boundary. */
    JS_SetMaxStackSize(rt, 0);
    JS_SetInterruptHandler(rt, expired, NULL);
    JSContext *ctx = JS_NewContext(rt);
    quantum();
    JSValue driver = JS_Eval(ctx, realm, size, "profile", JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(driver)) fail();
    char *line = NULL;
    size_t capacity = 0;
    int started = 0;
    while (getline(&line, &capacity, stdin) != -1) {
        if (strlen(line) > 131072) return 66;
        quantum();
        JSValue frame = JS_ParseJSON(ctx, line, strlen(line), "ipc");
        if (JS_IsException(frame)) fail();
        JSValue result;
        if (!started) {
            started = 1;
            JSValue kind = JS_GetPropertyStr(ctx, frame, "kind");
            const char *kindstr = JS_ToCString(ctx, kind);
            if (!kindstr || strcmp(kindstr, "start")) return 66;
            JS_FreeCString(ctx, kindstr); JS_FreeValue(ctx, kind);
            JSValue source = JS_GetPropertyStr(ctx, frame, "source");
            const char *text = JS_ToCString(ctx, source);
            if (!text || strlen(text) > 65536) return 66;
            size_t length = strlen(text) + 64;
            char *wrapped = malloc(length);
            if (!wrapped) fail();
            snprintf(wrapped, length, "(async function(args){\"use strict\";\n%s\n})", text);
            JSValue program = JS_Eval(ctx, wrapped, strlen(wrapped), "program", JS_EVAL_TYPE_GLOBAL);
            free(wrapped); JS_FreeCString(ctx, text); JS_FreeValue(ctx, source);
            if (JS_IsException(program)) fail();
            JSValue args = JS_GetPropertyStr(ctx, frame, "args");
            JSValue argsText = JS_JSONStringify(ctx, args, JS_UNDEFINED, JS_UNDEFINED);
            JS_FreeValue(ctx, args);
            JSValue argv[2] = {program, argsText};
            result = call(ctx, driver, "start", 2, argv);
            JS_FreeValue(ctx, argsText); JS_FreeValue(ctx, program);
        } else {
            JSValue delivery = JS_NewString(ctx, line);
            result = call(ctx, driver, "deliver", 1, &delivery);
            JS_FreeValue(ctx, delivery);
        }
        JS_FreeValue(ctx, result); JS_FreeValue(ctx, frame);
        JSContext *jobctx;
        int job, count = 0;
        while ((job = JS_ExecutePendingJob(rt, &jobctx)) > 0) {
            if (++count > 10000 || expired(rt, NULL)) fail();
        }
        if (job < 0 || expired(rt, NULL)) fail();
        result = call(ctx, driver, "take", 0, NULL);
        if (expired(rt, NULL)) fail();
        const char *encoded = JS_ToCString(ctx, result);
        if (!encoded) fail();
        // Parse the private driver's array and print one bounded JSON frame per line.
        JSValue batch = JS_ParseJSON(ctx, encoded, strlen(encoded), "frames");
        JS_FreeCString(ctx, encoded); JS_FreeValue(ctx, result);
        JSValue lengthValue = JS_GetPropertyStr(ctx, batch, "length");
        uint32_t length;
        if (JS_ToUint32(ctx, &length, lengthValue) < 0 || length > 129) fail();
        JS_FreeValue(ctx, lengthValue);
        for (uint32_t i = 0; i < length; i++) {
            if (expired(rt, NULL)) fail();
            JSValue item = JS_GetPropertyUint32(ctx, batch, i);
            JSValue serialized = JS_JSONStringify(ctx, item, JS_UNDEFINED, JS_UNDEFINED);
            const char *out = JS_ToCString(ctx, serialized);
            if (!out || strlen(out) > 65536) fail();
            puts(out);
            JS_FreeCString(ctx, out); JS_FreeValue(ctx, serialized); JS_FreeValue(ctx, item);
        }
        JS_FreeValue(ctx, batch);
        fflush(stdout);
    }
    free(line);
    JS_FreeValue(ctx, driver); JS_FreeContext(ctx); JS_FreeRuntime(rt);
    return 0;
}
