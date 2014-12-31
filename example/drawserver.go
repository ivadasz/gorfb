package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"

	"gorfb"
)

func drawdot(img draw.Image, pos image.Point) image.Rectangle {
	col := color.RGBA{50, 200, 150, 255}
	img.Set(pos.X, pos.Y, col)
	return image.Rect(pos.X, pos.Y, pos.X+1, pos.Y+1)
}

func main() {
	serv, err := gorfb.ServeDumbFb(":5900", 320, 240)
	if err != nil {
		log.Fatal(err)
	}
	defer serv.Shutdown()

	black := color.RGBA{0, 0, 0, 0}
	red := color.RGBA{255, 0, 0, 255}
	green := color.RGBA{0, 255, 0, 100}
	blue := color.RGBA{0, 0, 255, 255}

	img := <-serv.Getfb
	draw.Draw(img, image.Rect(0, 0, 320, 240), &image.Uniform{black}, image.ZP, draw.Src)
	draw.Draw(img, image.Rect(0, 0, 320, 80), &image.Uniform{red}, image.ZP, draw.Src)
	draw.Draw(img, image.Rect(0, 160, 320, 240), &image.Uniform{blue}, image.ZP, draw.Src)
	draw.Draw(img, image.Rect(0, 60, 320, 180), &image.Uniform{green}, image.ZP, draw.Over)
	serv.Relfb <- []image.Rectangle{image.Rect(0, 0, 320, 240)}

	pending := []gorfb.InputEvent{}
	for {
		// XXX when len(pending) becomes too large, start to throttle
		//     reading from "input chan InputEvent".
		// XXX instead of checking len(pending), check whether we
		//     actually want to draw something
		if len(pending) > 0 {
			select {
			case a := <-serv.Getfb:
				{
					dirty := []image.Rectangle{}
					for _, p := range pending {
						if p.T == 0 {
							// Mouse Event
							dirty = append(dirty, drawdot(a, p.Pos))
						} else {
							// Keyboard Event
							fmt.Printf("keyboard event: downflag=%v, key=0x%x\n", p.Mask, p.Key)
						}
					}
					serv.Relfb <- dirty
					pending = []gorfb.InputEvent{}
				}
			case a := <-serv.Input:
				{
					pending = append(pending, a)
				}
			case a := <-serv.Txt:
				{
					fmt.Printf("Cut Text: %v\n", a)
				}
			}
		} else {
			select {
			case a := <-serv.Input:
				{
					if a.T == 0 {
						// Mouse Event
						pending = append(pending, a)
					} else if a.T == 1 {
						// Keyboard Event
						fmt.Printf("keyboard event: downflag=%v, key=0x%x\n", a.Mask, a.Key)
					}
				}
			case a := <-serv.Txt:
				{
					fmt.Printf("Cut Text: %v\n", a)
				}
			}
		}
	}
}
