---
owner: function:ns_match
revision: 2
epoch: 1
complete: false
answers: ["need:function:ns_match->struct:ns_common@r1"]
resolves: ["reply:function:ns_match->struct:ns_common@abc123def456"]
retired: [C7]
author_model: openai-codex/gpt-5.6-terra
---
1. [C1] ns_match returns 1 when the namespace number and device match. uses: struct:ns_common#C2, global:nsfs_mnt@fs/nsfs.c#C1 by: code fs/nsfs.c:400
    1. [C2] The namespace number read returns the value that initialization wrote. uses: struct:ns_common#C2 by: uses
