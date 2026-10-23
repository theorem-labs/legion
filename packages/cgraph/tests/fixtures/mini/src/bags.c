#include "mini.h"

struct list_node all_bags;
extern const struct ops box_ops;

int ext_check(struct box *b)
{
	return b->size > 0;
}

static int visit(struct box *b)
{
	return b->size > 1;
}

__attribute__((noinline)) int walk(int (*fn)(struct box *), struct box *b)
{
	return fn(b);
}

int bag_add(struct bag *g, struct item *it)
{
	list_insert(&it->link, &g->all);
	g->n++;
	return g->n;
}

void bag_link(struct bag *g)
{
	list_insert(&g->all, &all_bags);
}

int bag_count(struct bag *g)
{
	typeof(tree_sem.count) total = 0;

	total += walk(visit, current_box);
	total += current_box->size;
	total += pick_list(g) != NULL;
	total += run_get(&box_ops, current_box);
	return total;
}

static __attribute__((noinline)) int helper_dup(int x)
{
	return x * 2;
}

int bags_dup(int x)
{
	return helper_dup(x) + walk(ext_check, NULL);
}

/* for_each_box(b) would walk box_table; the macro is a membership witness only. */
long crate_size(struct crate *c)
{
	return c->box.size + c->tag;
}

int arch_hook(void)
{
	return 2;
}

__attribute__((noinline)) int bag_weight(void)
{
	return 3;
}

int stub_open(void)
{
	return bag_weight();
}
