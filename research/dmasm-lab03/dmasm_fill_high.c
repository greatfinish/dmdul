#define _POSIX_C_SOURCE 200112L

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Minimal declarations for the mirror API shipped with the DM8 test build. */
typedef signed char sdbyte;
typedef unsigned char udbyte;
typedef int32_t sdint4;
typedef uint16_t udint2;
typedef uint32_t udint4;
typedef uint64_t udint8;
typedef udint4 asmbool;
typedef sdint4 ASMRETURN;
typedef udint4 asm_fhandle_t;
typedef void *asmcon_handle;

extern ASMRETURN dmasmm_sys_init(sdbyte *, udint4 *, udint4, udint4);
extern void dmasmm_sys_deinit(void);
extern ASMRETURN dmasmm_alloc_con(asmcon_handle *, sdbyte *, udint4 *);
extern void dmasmm_free_con(asmcon_handle);
extern ASMRETURN dmasmm_connect(asmcon_handle, sdbyte *, sdbyte *, sdbyte *,
                               udint2, asmbool *, sdbyte *, udint4 *);
extern void dmasmm_close_con(asmcon_handle);
extern ASMRETURN dmasmm_file_open(asmcon_handle, sdbyte *, asm_fhandle_t *,
                                 sdbyte *, udint4 *);
extern ASMRETURN dmasmm_file_close(asmcon_handle, asm_fhandle_t);
extern ASMRETURN dmasmm_file_write_by_offset(asmcon_handle, asm_fhandle_t,
                                             udint8, sdbyte *, udint4,
                                             sdbyte *, udint4 *);

#define CHUNK_SIZE (1024U * 1024U)
#define PAGE_SIZE 4096U
#define ERR_SIZE 1024U
#define OK(code) ((sdint4)(code) >= 0)

struct sample_file {
    const char *path;
    uint64_t size_mb;
    uint64_t tag;
    unsigned char fill;
};

static const struct sample_file samples[] = {
    {"+HIGH4/high_stripe0_au4.dat", 256, 0x4849474830000001ULL, 0x48},
    {"+HIGH4/high_stripe32_au4.dat", 256, 0x4849474833320002ULL, 0x49},
};

static void reset_error(sdbyte *desc, udint4 *len)
{
    memset(desc, 0, ERR_SIZE);
    *len = ERR_SIZE - 1;
}

static int fail(asmcon_handle conn, unsigned char *buffer, const char *message,
                ASMRETURN code, const sdbyte *description)
{
    fprintf(stderr, "%s: %d %s\n", message, code, description);
    free(buffer);
    if (conn != NULL) {
        dmasmm_close_con(conn);
        dmasmm_free_con(conn);
    }
    dmasmm_sys_deinit();
    return 1;
}

int main(int argc, char **argv)
{
    const char *host = argc > 1 ? argv[1] : "127.0.0.1";
    unsigned long port_value = argc > 2 ? strtoul(argv[2], NULL, 10) : 9351;
    const char *password = getenv("DMASM_PASSWORD");
    sdbyte err_desc[ERR_SIZE];
    udint4 err_len;
    ASMRETURN code;
    asmcon_handle conn = NULL;
    asmbool is_local = 0;
    unsigned char *buffer = NULL;
    size_t i;

    if (port_value > 65535) {
        fprintf(stderr, "invalid port: %lu\n", port_value);
        return 2;
    }
    if (password == NULL) {
        password = "local-login";
    }

    reset_error(err_desc, &err_len);
    code = dmasmm_sys_init(err_desc, &err_len, 1, 0);
    if (!OK(code)) {
        fprintf(stderr, "dmasmm_sys_init failed: %d %s\n", code, err_desc);
        return 1;
    }

    reset_error(err_desc, &err_len);
    code = dmasmm_alloc_con(&conn, err_desc, &err_len);
    if (!OK(code)) {
        dmasmm_sys_deinit();
        fprintf(stderr, "dmasmm_alloc_con failed: %d %s\n", code, err_desc);
        return 1;
    }

    reset_error(err_desc, &err_len);
    code = dmasmm_connect(conn, (sdbyte *)"ASMSYS", (sdbyte *)password,
                         (sdbyte *)host, (udint2)port_value, &is_local,
                         err_desc, &err_len);
    if (!OK(code)) {
        return fail(conn, buffer, "dmasmm_connect failed", code, err_desc);
    }
    if (!is_local) {
        fprintf(stderr, "refusing mirror writes through a non-local connection\n");
        dmasmm_close_con(conn);
        dmasmm_free_con(conn);
        dmasmm_sys_deinit();
        return 1;
    }

    if (posix_memalign((void **)&buffer, PAGE_SIZE, CHUNK_SIZE) != 0) {
        fprintf(stderr, "posix_memalign failed: %s\n", strerror(errno));
        dmasmm_close_con(conn);
        dmasmm_free_con(conn);
        dmasmm_sys_deinit();
        return 1;
    }

    for (i = 0; i < sizeof(samples) / sizeof(samples[0]); ++i) {
        asm_fhandle_t handle;
        uint64_t size = samples[i].size_mb * 1024ULL * 1024ULL;
        uint64_t offset;

        reset_error(err_desc, &err_len);
        code = dmasmm_file_open(conn, (sdbyte *)samples[i].path, &handle,
                               err_desc, &err_len);
        if (!OK(code)) {
            return fail(conn, buffer, "dmasmm_file_open failed", code,
                        err_desc);
        }

        for (offset = 0; offset < size; offset += CHUNK_SIZE) {
            udint4 bytes = (udint4)((size - offset) < CHUNK_SIZE
                                        ? (size - offset)
                                        : CHUNK_SIZE);
            udint4 page;

            memset(buffer, samples[i].fill, bytes);
            for (page = 0; page < bytes; page += PAGE_SIZE) {
                uint64_t *header = (uint64_t *)(buffer + page);
                header[0] = samples[i].tag;
                header[1] = offset + page;
            }

            reset_error(err_desc, &err_len);
            code = dmasmm_file_write_by_offset(conn, handle, offset,
                                               (sdbyte *)buffer, bytes,
                                               err_desc, &err_len);
            if (!OK(code)) {
                dmasmm_file_close(conn, handle);
                return fail(conn, buffer, "dmasmm_file_write_by_offset failed",
                            code, err_desc);
            }
        }

        dmasmm_file_close(conn, handle);
        printf("initialized %s (%llu MiB)\n", samples[i].path,
               (unsigned long long)samples[i].size_mb);
        fflush(stdout);
    }

    free(buffer);
    dmasmm_close_con(conn);
    dmasmm_free_con(conn);
    dmasmm_sys_deinit();
    return 0;
}
