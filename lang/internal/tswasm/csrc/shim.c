// shim.c is the whole ABI between Go and the tree-sitter runtime compiled to
// WebAssembly. One call parses a file and writes the syntax tree as a flat
// array of fixed-size records, so Go makes one call per parse instead of one
// call per node.
//
// Record layout (6 x uint32 per node, preorder, anonymous nodes included):
//   [0] symbol | field_id << 16 | flags << 24
//   [1] start byte   [2] end byte
//   [3] start row    [4] end row (both 0-based)
//   [5] parent record index, or 0xFFFFFFFF for the root

#include <stdint.h>
#include <stdlib.h>

#include "tree_sitter/api.h"

const TSLanguage *tree_sitter_go(void);
const TSLanguage *tree_sitter_typescript(void);
const TSLanguage *tree_sitter_tsx(void);

#define NLANG 3
#define REC 6

enum {
  FLAG_NAMED = 1,
  FLAG_EXTRA = 2,
  FLAG_ERROR = 4,
  FLAG_MISSING = 8,
  FLAG_HAS_ERROR = 16,
};

static const TSLanguage *language(uint32_t id) {
  switch (id) {
    case 0: return tree_sitter_go();
    case 1: return tree_sitter_typescript();
    case 2: return tree_sitter_tsx();
  }
  return NULL;
}

static TSParser *parsers[NLANG];
static uint32_t *out;
static uint32_t out_cap; // records
static uint32_t *stack;
static uint32_t stack_cap;

__attribute__((export_name("bk_alloc"))) void *bk_alloc(uint32_t n) { return malloc(n); }
__attribute__((export_name("bk_free"))) void bk_free(void *p) { free(p); }

__attribute__((export_name("bk_symbol_count"))) uint32_t bk_symbol_count(uint32_t id) {
  const TSLanguage *l = language(id);
  return l ? ts_language_symbol_count(l) : 0;
}

__attribute__((export_name("bk_symbol_name"))) const char *bk_symbol_name(uint32_t id, uint32_t sym) {
  return ts_language_symbol_name(language(id), (TSSymbol)sym);
}

__attribute__((export_name("bk_field_count"))) uint32_t bk_field_count(uint32_t id) {
  const TSLanguage *l = language(id);
  return l ? ts_language_field_count(l) : 0;
}

__attribute__((export_name("bk_field_name"))) const char *bk_field_name(uint32_t id, uint32_t field) {
  return ts_language_field_name_for_id(language(id), (TSFieldId)field);
}

static int grow(uint32_t **p, uint32_t *cap, uint32_t need, uint32_t width) {
  if (need <= *cap) return 1;
  uint32_t n = *cap ? *cap : 4096;
  while (n < need) n *= 2;
  uint32_t *q = realloc(*p, (size_t)n * width * sizeof(uint32_t));
  if (!q) return 0;
  *p = q;
  *cap = n;
  return 1;
}

static int emit(TSTreeCursor *c, uint32_t parent, uint32_t *count) {
  if (!grow(&out, &out_cap, *count + 1, REC)) return 0;
  TSNode n = ts_tree_cursor_current_node(c);
  uint32_t flags = 0;
  if (ts_node_is_named(n)) flags |= FLAG_NAMED;
  if (ts_node_is_extra(n)) flags |= FLAG_EXTRA;
  if (ts_node_is_error(n)) flags |= FLAG_ERROR;
  if (ts_node_is_missing(n)) flags |= FLAG_MISSING;
  if (ts_node_has_error(n)) flags |= FLAG_HAS_ERROR;
  uint32_t field = ts_tree_cursor_current_field_id(c) & 0xFF;
  uint32_t *r = out + (size_t)(*count) * REC;
  r[0] = (uint32_t)ts_node_symbol(n) | field << 16 | flags << 24;
  r[1] = ts_node_start_byte(n);
  r[2] = ts_node_end_byte(n);
  r[3] = ts_node_start_point(n).row;
  r[4] = ts_node_end_point(n).row;
  r[5] = parent;
  (*count)++;
  return 1;
}

// bk_parse parses len bytes at src with language id and returns a pointer to
// the records, writing the record count to *out_count. The records stay valid
// until the next call. It returns 0 on failure.
__attribute__((export_name("bk_parse"))) uint32_t *bk_parse(uint32_t id, const char *src, uint32_t len, uint32_t *out_count) {
  *out_count = 0;
  const TSLanguage *l = language(id);
  if (!l) return 0;
  if (!parsers[id]) {
    parsers[id] = ts_parser_new();
    if (!ts_parser_set_language(parsers[id], l)) return 0;
  }
  TSTree *tree = ts_parser_parse_string(parsers[id], NULL, src, len);
  if (!tree) return 0;

  uint32_t count = 0, depth = 0;
  int ok = 1;
  TSTreeCursor c = ts_tree_cursor_new(ts_tree_root_node(tree));
  ok = emit(&c, 0xFFFFFFFF, &count) && grow(&stack, &stack_cap, 1, 1);
  if (ok) stack[depth++] = 0;
  while (ok) {
    if (ts_tree_cursor_goto_first_child(&c)) {
      uint32_t idx = count;
      ok = emit(&c, stack[depth - 1], &count) && grow(&stack, &stack_cap, depth + 1, 1);
      if (ok) stack[depth++] = idx;
      continue;
    }
    while (!ts_tree_cursor_goto_next_sibling(&c)) {
      if (!ts_tree_cursor_goto_parent(&c)) goto done;
      depth--;
    }
    // Moved to a sibling: replace the current node on the stack.
    depth--;
    uint32_t idx = count;
    ok = emit(&c, stack[depth - 1], &count);
    stack[depth++] = idx;
  }
done:
  ts_tree_cursor_delete(&c);
  ts_tree_delete(tree);
  if (!ok) return 0;
  *out_count = count;
  return out;
}
