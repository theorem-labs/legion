namespace Gold

theorem frame_al {sp : BitVec 64} (h : Frame sp) : Aligned sp := h.al

theorem frame_al_copy {sp : BitVec 64} (h : Frame sp) : Aligned sp := h.al

end Gold
