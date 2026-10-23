#include "mini.h"

struct list_node box_table[16];
struct seqlk table_lock;
struct rwsem tree_sem;
static int box_count;
static struct box *last;
static const int tree_step = 1;

static int box_get(struct box *b)
{
	return b->b.refs++;
}

static void box_put(struct box *b)
{
	b->b.refs--;
}

const struct ops box_ops = {
	.get = box_get,
	.put = box_put,
};

static int next_id(void)
{
	static int counter;

	return ++counter;
}

static void hash_box(struct box *b, unsigned long key)
{
	write_seqlk(&table_lock);
	hash_insert(&b->hash, bucket_of(key));
	write_sequnlk(&table_lock);
}

struct box *box_new(unsigned long key)
{
	struct box *b = obj_alloc(sizeof(struct box));

	if (!b)
		return NULL;
	CHECK_VALUE(box_count);
	b->size = sizeof(last->size);
	b->owner.val = next_id();
	hash_box(b, key);
	box_count++;
	if (FEATURE_X)
		last = b;
	b->size += box_size(b);
	return b;
}

void box_tree_add(struct box *b, struct bag *g)
{
	guard_tree();
	b->first = NULL;
	g->n += tree_step;
}

int box_check(struct box *b)
{
	return ext_check(b);
}

void box_free(struct box *b)
{
	obj_free(b);
}

static __attribute__((noinline)) int helper_dup(int x)
{
	return x + box_count;
}

int store_dup(int x)
{
	return helper_dup(x);
}

int run_get(const struct ops *o, struct box *b)
{
	return o->get(b);
}

__attribute__((weak)) int arch_hook(void)
{
	return 0;
}

COND_STUB(open)

int use_hook(void)
{
	return arch_hook() + stub_open();
}

__attribute__((noinline)) int box_weight(void)
{
	return ++box_count;
}

__attribute__((noinline)) void tree_unlock(struct rwsem **s)
{
	up_write(*s);
}

int box_scoped(struct box *b)
{
	LOCK_TREE(held);
	struct box *spare __free(obj) = obj_alloc(sizeof(*spare));

	return weight_of(b) + (int)size_of(b) + (spare != NULL);
}
