/* State accesses the graph cannot hold, each kept in exclusions.json with its reason (contract
 * C3): a lock that is the function's own parameter (parameter_at_root), a lock a folded callee
 * takes through a pointer its caller cannot name (unbound_parameter), and a field read in a
 * _Generic association whose selection the facts could not read (generic_unconfirmed). A lock
 * reached through a member of the caller's parameter is held: it is an edge, not an exclusion. */

void spin_lock(void *lock);
void spin_unlock(void *lock);

struct guard {
	long count;
};

struct holder {
	struct guard *outer;
	long tag;
};

struct guard *fetch_guard(void);

/* The lock this function takes is its own parameter: no state of the program names it. */
long take_parameter(struct guard *g, long value)
{
	spin_lock(g);
	spin_unlock(g);
	return value + 1;
}

static inline void take_inner(struct guard *g)
{
	spin_lock(g);
	spin_unlock(g);
}

/* The folded callee's lock is a member of this caller's parameter: the graph holds it. */
long take_loaded(struct holder *h)
{
	take_inner(h->outer);
	return h->tag;
}

/* The folded callee's lock is the value of a call: the caller binds its parameter to nothing. */
long take_fetched(void)
{
	take_inner(fetch_guard());
	return 0;
}

struct common {
	long state;
};

struct ring {
	struct common ns;
	long head;
};

struct other {
	struct common ns;
	long tail;
};

/* A _Generic the facts cannot read: a macro writes its association list, so the text of the
 * selection cannot be read as a table of types, and both associations have the type of the whole
 * (struct common *), so the type of the selection names neither. Which struct the read names is
 * unknown, and the access is kept with its reason instead of being held. */
#define RING_OR_OTHER struct ring *: &r->ns, struct other *: (struct common *)0

long active_ring(struct ring *r)
{
	return _Generic((r), RING_OR_OTHER)->state;
}
