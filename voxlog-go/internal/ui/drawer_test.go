package ui

import "testing"

func TestDrawerPlacementFallsBackToTheNotch(t *testing.T) {
	// An unset or hand-edited setting must not leave the drawer somewhere
	// the placement code has no case for -- it would open at whatever frame
	// the window happened to be created with.
	for _, in := range []string{"", "cursor", "follow", "screen_bottom_left", "nonsense"} {
		if got := normalizeDrawerPlacement(in); got != DefaultDrawerPlacement {
			t.Errorf("normalizeDrawerPlacement(%q) = %q, want %q", in, got, DefaultDrawerPlacement)
		}
	}
}

func TestDrawerPlacementKeepsEveryOfferedValue(t *testing.T) {
	for _, p := range DrawerPlacements {
		if got := normalizeDrawerPlacement(p); got != p {
			t.Errorf("normalizeDrawerPlacement(%q) = %q, want it unchanged", p, got)
		}
	}
}

func TestEveryDrawerPlacementCanBePlaced(t *testing.T) {
	// placeDrawer resolves a placement through overlayCorners or the two
	// centred cases; anything else silently lands at the top centre. Catch a
	// value added to the offered list but not to the map.
	for _, p := range DrawerPlacements {
		if p == OverlayTopCentre || p == OverlayBottomCentre {
			continue
		}
		if _, ok := overlayCorners[p]; !ok {
			t.Errorf("placement %q is offered but has no corner to place it in", p)
		}
	}
}
