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
/* Only crate embeds box, so the embedding joins them; crate is the larger struct. */
struct crate {
	struct box box;
	long tag;
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
#define CHECK_VALUE(e) ((void)sizeof(e))
#define FEATURE_X 0

/* Per-CPU state: the current box lives in the thread pointer. */
register struct box *current_box __asm__("tp");

#endif
