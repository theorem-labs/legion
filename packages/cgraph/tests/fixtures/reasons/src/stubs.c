/* Bodies for the lock helpers, so the program links. No part names this file. */

static volatile long taken;

void spin_lock(void *lock)
{
	taken += (long)lock;
}

void spin_unlock(void *lock)
{
	taken -= (long)lock;
}

struct guard;
static long slot;

struct guard *fetch_guard(void)
{
	return (struct guard *)&slot;
}

void _start(void)
{
	for (;;)
		;
}
