/* A small program with the shapes the graph must handle: embedding, list and hash membership,
 * locks, a cleanup class, unevaluated operands, a callback table, per-CPU state. */
#ifndef MINI_H
#define MINI_H

#define NULL ((void *)0)
typedef unsigned long size_t;

/* A value: no pointer at any depth. */
typedef struct {
	unsigned int val;
} uid_v;

/* A kit type: accesses through it belong to the object that holds it. */
struct list_node {
	struct list_node *next, *prev;
};

/* Lock types. */
struct spin {
	int v;
};
struct rwsem {
	struct spin wait;
	long count;
};
struct seqlk {
	struct spin lock;
	unsigned int seq;
};

/* A base that two types embed. */
struct base {
	int refs;
	struct list_node tree;
};

struct item;
struct box {
	struct base b;
	uid_v owner;
	struct list_node hash;
	struct item *first;
	long size;
};
struct bag {
	struct base b;
	struct list_node all;
	int n;
};
struct item {
	struct list_node link;
	struct box *parent;
	int value;
};
/* Only crate embeds box, so the embedding joins them; crate is the larger struct. box sits at
 * offset 8, so c->box.size is 8 + 56 bytes from c. */
struct crate {
	long tag;
	struct box box;
};
struct ops {
	int (*get)(struct box *);
	void (*put)(struct box *);
};

extern struct list_node box_table[16];
extern struct seqlk table_lock;
extern struct rwsem tree_sem;
extern struct list_node all_bags;

void spin_lock(struct spin *s);
void spin_unlock(struct spin *s);
void down_write(struct rwsem *s);
void up_write(struct rwsem *s);
void *obj_alloc(size_t size);
void obj_free(void *p);
int ext_check(struct box *b);
int walk(int (*fn)(struct box *), struct box *b);
int run_get(const struct ops *o, struct box *b);

static inline void list_insert(struct list_node *n, struct list_node *head)
{
	n->next = head->next;
	n->prev = head;
	head->next = n;
}

static inline void hash_insert(struct list_node *n, struct list_node *bucket)
{
	list_insert(n, bucket);
}

static inline struct list_node *bucket_of(unsigned long key)
{
	return &box_table[key & 15];
}

static inline void write_seqlk(struct seqlk *s)
{
	spin_lock(&s->lock);
	s->seq++;
}

static inline void write_sequnlk(struct seqlk *s)
{
	s->seq++;
	spin_unlock(&s->lock);
}

/* A header helper that the compiler keeps out of line. */
static __attribute__((noinline)) long box_size(struct box *b)
{
	return b->size;
}

/* A cleanup class: the guard takes tree_sem and its destructor releases it. */
typedef struct rwsem *class_tree_t;
static inline void class_tree_destructor(class_tree_t *p)
{
	up_write(*p);
}
static inline class_tree_t class_tree_constructor(struct rwsem *s)
{
	down_write(s);
	return s;
}
#define guard_tree() \
	class_tree_t __guard __attribute__((cleanup(class_tree_destructor))) = class_tree_constructor(&tree_sem)

#define pick_list(x) _Generic((x), struct box *: &box_table[0], struct bag *: &all_bags)
/* _Generic in a macro that selects a call and a struct access: only the selected arm is code. */
int box_weight(void);
int bag_weight(void);
#define weight_of(x) _Generic((x), struct box *: box_weight(), struct bag *: bag_weight())
#define size_of(x) _Generic((x), struct box *: ((struct box *)(void *)(x))->size, \
	struct bag *: (long)((struct bag *)(void *)(x))->n)
#define inner_weight(x) _Generic((x), struct box *: box_weight(), struct bag *: bag_weight())
#define outer_weight(x) inner_weight(x)
/* _Generic choosing the function a call then calls, as seqlock.h's __seqprop: a call, not an address. */
static inline long box_measure(struct box *b)
{
	return b->size;
}
static inline long bag_measure(struct bag *g)
{
	return g->n;
}
#define measure(x) _Generic((x), struct box *: box_measure, struct bag *: bag_measure)(x)
/* Associations a macro pastes together, which no text reading resolves (seqlock.h's __seqprop_case). */
#define measure_case(kind) struct kind *: kind##_measure
#define measure_any(x) _Generic((x), measure_case(box), measure_case(bag))(x)

/* Cleanup attributes outside the class convention: a free helper and a plain cleanup function. */
#define __cleanup(func) __attribute__((__cleanup__(func)))
#define __free(name) __cleanup(__free_##name)
static inline void __free_obj(void *p)
{
	obj_free(*(void **)p);
}
void tree_unlock(struct rwsem **s);
#define LOCK_TREE(s) struct rwsem *s __cleanup(tree_unlock) = &tree_sem

/* A default the architecture may replace: the strong definition in bags.c wins at link time. */
int arch_hook(void);
/* A weak stub a macro generates, as a table of optional system calls does. */
#define __weak __attribute__((__weak__))
/* include/linux/compiler_types.h: an attribute macro that can come first in a definition. */
#define notrace __attribute__((__no_instrument_function__))
#define COND_STUB(name) int __weak stub_##name(void) { return -1; }
int stub_open(void);
#define CHECK_VALUE(e) ((void)sizeof(e))
#define FEATURE_X 0

/* Operations a table or a function names by address and nothing calls, as filelock.h's
 * generic_setlease without file locking: the address gives each a copy in the unit that names it. */
static inline int box_get_default(struct box *b)
{
	return b != NULL;
}
static inline int box_get_none(struct box *b)
{
	(void)b;
	return 0;
}
/* A clock that a function calls, inlined, and a pointer names, as timekeeping.h's ktime_get_real. */
static inline long clock_now(void)
{
	return 5;
}
/* A GNU inline definition: every includer sees it and none emits it, so no unit owns it. */
extern inline __attribute__((__gnu_inline__)) int box_inline_default(void)
{
	return 4;
}

/* Per-CPU state: the current box lives in the thread pointer. */
register struct box *current_box __asm__("tp");

/* include/linux/list.h list_for_each_entry: a for loop a header's macro writes. */
#define dd_first(head) ((head)->next)
#define dd_for_each(pos, head) for (pos = dd_first(head); pos != (head); pos = pos->next)

#endif
