/* One of two units that each define a static `tally` of their own, with different parameter
 * counts. The image holds a copy of each, so the decoder must read each copy's own signature:
 * the number of parameters decides which argument registers a call passes, and so which formed
 * addresses escape into a callee. */

struct left {
	long head;
	long tail;
};

struct left left_state;

static __attribute__((noinline)) long tally(long a)
{
	return a + left_state.tail;
}

/* The volatile pointer keeps the copy of this unit out of line and its address taken. */
long (*volatile left_hook)(long) = tally;

long left_entry(long k)
{
	return tally(k) + tally(k + 7);
}
