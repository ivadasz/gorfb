package gorfb

import (
	"image"
)

type (
	Dirty struct {
		a image.Rectangle
		b image.Rectangle
	}
)

func (d Dirty) add(update image.Rectangle) Dirty {
	if d.empty() {
		return Dirty{update, d.b}
	} else if !d.a.Intersect(update).Empty() {
		if d.b.Intersect(update).Empty() {
			return Dirty{d.a.Union(update), d.b}
		} else {
			return Dirty{d.a.Union(update).Union(d.b), image.Rect(0, 0, 0, 0)}
		}
	} else if !d.b.Intersect(update).Empty() {
		return Dirty{d.a, d.b.Union(update)}
	} else if !d.b.Empty() {
		return Dirty{d.b.Union(update), d.a}
	} else {
		return Dirty{update, d.a}
	}
}

func (d Dirty) intersect(rect image.Rectangle) Dirty {
	return Dirty{d.a.Intersect(rect), d.b.Intersect(rect)}
}

func (d Dirty) empty() bool {
	return d.a.Empty() && d.b.Empty()
}

func mkclean() Dirty {
	return Dirty{image.Rect(0, 0, 0, 0), image.Rect(0, 0, 0, 0)}
}

func (d Dirty) toRects() []image.Rectangle {
	if d.a.Empty() && d.b.Empty() {
		return []image.Rectangle{}
	} else if d.a.Empty() {
		return []image.Rectangle{d.b}
	} else if d.b.Empty() {
		return []image.Rectangle{d.a}
	} else {
		return []image.Rectangle{d.a, d.b}
	}
}
