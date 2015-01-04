package gorfb

import (
	"fmt"
)

type (
	Syncer struct {
		Killer <-chan interface{}
		Dead   chan<- string
		add    chan<- string
		done   <-chan interface{}
	}
)

func (s Syncer) Add(n string) {
	s.add <- n
}

func (s Syncer) Wait() {
	defer fmt.Printf("Wait() is done\n")
	<-s.done
}

func (s Syncer) Killed(n string) {
	s.Dead <- n
}

func NewSyncer() Syncer {
	k := make(chan interface{})
	d := make(chan string)
	add := make(chan string)
	done := make(chan interface{})

	go func() {
		cnt := 0

		for {
			select {
			case <-add:
				{
					cnt++
				}
			case name := <-d:
				{
					fmt.Printf("dead goroutine: %s\n", name)
					cnt--
					for {
						if cnt == 0 {
							close(k)
							close(d)
							close(add)
							done <- nil
							close(done)
							return
						}
						select {
						case <-add:
							{
								cnt++
							}
						case name := <-d:
							{
								fmt.Printf("dead goroutine: %s\n", name)
								cnt--
							}
						case k <- nil:
						}
					}
				}
			}
		}
	}()

	return Syncer{k, d, add, done}
}
