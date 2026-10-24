/* Code the program patches when it starts, for the CPU it runs on: the kernel's alternatives
 * (arch/riscv/include/asm/alternative-macros.h). The patch table lists each site, an instruction
 * that boot code may overwrite with a replacement of the same length, which the assembler keeps
 * after the file's functions (.subsection 1). The image links this file after src/fold.c. Its
 * global is in .data, so the stubs' heap still ends .bss, and its table is writable, so the image
 * places it among the data, after .text, and .text keeps its addresses. No part names this file:
 * only the image tests read it. */

/* ALT_ENTRY: the site and the replacement as offsets from their own fields, a u16 vendor, the u16
 * length of the replacement, and a u32 patch number (52 is the kernel's RISCV_ISA_EXT_ZKR). */
#define PATCH(old, new, feature)                                                                \
	"886:\n.option push\n.option norvc\n" old "\n.option pop\n887:\n"                         \
	".pushsection .alternative, \"aw\"\n"                                                     \
	".4byte 886b - .\n.4byte 888f - .\n.2byte 0\n.2byte 889f - 888f\n.4byte " #feature "\n" \
	".popsection\n"                                                                           \
	".subsection 1\n888:\n.option push\n.option norvc\n" new "\n.option pop\n889:\n.previous\n"

/* __riscv_has_extension_likely: a jump past the feature's code, which the patch makes a nop. */
static inline __attribute__((always_inline)) _Bool patch_has_feature(void)
{
	asm goto(PATCH("j %l[no]", "nop", 52) : : : : no);
	return 1;
no:
	return 0;
}

void fct_show(unsigned long mask);
void *fct_cur(void);

static _Bool patch_reported __attribute__((section(".data")));

/* drivers/char/random.c extract_entropy, with arch_get_random_seed_longs and csr_seed_long inlined:
 * pr_err_once's __already_done is read and set only on the path the patch opens, through a
 * register the compiler sets before the loop. */
void patch_fill(unsigned long *v, int n)
{
	for (int i = 0; i < n; i++) {
		if (patch_has_feature() && !patch_reported) {
			patch_reported = 1;
			fct_show(i);
		}
		v[i] = (unsigned long)fct_cur();
	}
}
