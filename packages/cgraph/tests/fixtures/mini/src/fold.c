#include "mini.h"

/* Code whose facts depend on what the code around it does with it: mostly helpers the image inlines,
 * as link folds them into their callers, where what a caller does with a call decides which of the
 * helper's events are code there. Each function is a shape the kernel has; the comment names one
 * kernel site. The image links this file last, after the stubs, so the code of the files before it
 * keeps its addresses. It has no constant data, which would come before them, and no global, as the
 * image's .bss ends with the stubs' heap, which the image tests read. */

/* include/linux/container_of.h, as src/facts.c gives it. */
#define container_of(ptr, type, member) ({ \
	void *__mptr = (void *)(ptr); \
	_Static_assert(__builtin_types_compatible_p(typeof(*(ptr)), typeof(((type *)0)->member)), \
		       "pointer type mismatch in container_of()"); \
	((type *)(__mptr - __builtin_offsetof(type, member))); })

struct fd_in {
	long a[4];
};
struct fd_out {
	long pad[4];
	struct fd_in m;
	long *ns;
	struct list_node link;
};

/* drivers/base/component.c __aggregate_find: list_for_each_entry(m, &aggregate_devices, node) if
 * (m->parent == parent && ...) return m; return NULL;. The loop stores only its cursor, but it may
 * leave the function with it: the loop is code. */
struct fd_out *fd_find(struct list_node *head, long *ns)
{
	struct list_node *pos;

	dd_for_each(pos, head)
		if (container_of(pos, struct fd_out, link)->ns == ns)
			return container_of(pos, struct fd_out, link);
	return NULL;
}

/* kernel/sys.c validate_prctl_map_addr: for (i = 0; i < ARRAY_SIZE(offsets); i++) { ... if (val >=
 * mmap_max_addr || val < mmap_min_addr) goto out; }: a loop that may jump out is code. */
struct fd_limits {
	long low;
	long v[4];
};

int fd_check_all(struct fd_limits *l)
{
	int i;

	for (i = 0; i < 4; i++)
		if (l->v[i] < l->low)
			goto out;
	return 0;
out:
	return -1;
}
