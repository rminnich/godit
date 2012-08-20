package main

import (
	"bytes"
	"github.com/nsf/termbox-go"
	"github.com/nsf/tulib"
	"io"
	"os/exec"
	"strconv"
	"unicode/utf8"
)

//----------------------------------------------------------------------------
// extended cursor location (includes absolute bytes offset)
//----------------------------------------------------------------------------

type cursor_location_ex struct {
	cursor_location
	abs_boffset int
}

func make_cursor_location_ex(cursor cursor_location) cursor_location_ex {
	off := cursor.boffset
	line := cursor.line.prev
	for line != nil {
		off += len(line.data) + 1 // plus one is for '\n'
		line = line.prev
	}
	return cursor_location_ex{
		cursor_location: cursor,
		abs_boffset:     off,
	}
}

//----------------------------------------------------------------------------
// autocompletion
//----------------------------------------------------------------------------

const ac_max_filtered = 200
const ac_ui_max_lines = 14

type ac_proposal struct {
	display []byte
	content []byte
}

type ac_func func(path string, r io.Reader, cursor cursor_location_ex) ([]ac_proposal, int)

type autocompl struct {
	// data
	origin    cursor_location
	current   cursor_location
	proposals []ac_proposal
	filtered  []ac_proposal

	// ui
	cursor int
	view   int
	tmpbuf bytes.Buffer
}

// Creates a new autocompletion object and makes a query for ac proposals, may
// take a while.
func new_autocompl(f ac_func, view *view) *autocompl {
	var charsback int

	origin_ex := make_cursor_location_ex(view.cursor)
	ac := new(autocompl)
	ac.filtered = make([]ac_proposal, 0, ac_max_filtered)
	ac.proposals, charsback = f(view.buf.path, view.buf.reader(), origin_ex)
	if len(ac.proposals) == 0 {
		return nil
	}

	if charsback > 0 {
		origin := view.cursor

		// adjust origin if we have positive 'charsback'
		for charsback > 0 {
			view.move_cursor_backward()
			charsback--
		}

		// delete region between the origin and the new cursor position
		view.action_delete(view.cursor, view.cursor.distance(origin))
		view.finalize_action_group()
	}
	ac.origin = view.cursor
	ac.current = view.cursor

	// insert the common part of all the autocompletion proposals
	common := ac.common()
	if len(common) > 0 {
		c := view.cursor
		view.action_insert(c, common)
		c.boffset += len(common)
		view.move_cursor_to(c)
		view.finalize_action_group()
		ac.update(view.cursor)
	}
	return ac
}

func (ac *autocompl) common() []byte {
	common := ac.proposals[0].content
	common_n := len(common)
	for _, p := range ac.proposals {
		if len(p.content) < common_n {
			common_n = len(p.content)
		}

		for i := 0; i < common_n; i++ {
			if common[i] != p.content[i] {
				common_n = i
				break
			}
		}
	}

	return clone_byte_slice(common[:common_n])
}

func (ac *autocompl) actual_proposals() []ac_proposal {
	if ac.origin.boffset != ac.current.boffset {
		return ac.filtered
	}
	return ac.proposals
}

// Returns 'true' if update was successful, 'false' if autocompletion should be
// discarded.
func (ac *autocompl) update(current cursor_location) bool {
	if ac.origin.line_num != current.line_num {
		return false
	}
	if ac.origin.boffset > current.boffset {
		return false
	}

	if ac.current.boffset == current.boffset {
		// false update, skip it
		return true
	}

	ac.current = current
	if ac.current.boffset == ac.origin.boffset {
		// simply discard filtered stuff
		return true
	}

	ac.filtered = ac.filtered[:0]
	filter := bytes_between(ac.origin, ac.current)
	j := 0
	for i := 0; i < ac_max_filtered; i++ {
		if j >= len(ac.proposals) {
			break
		}
		if bytes.HasPrefix(ac.proposals[j].content, filter) {
			ac.filtered = append(ac.filtered, ac.proposals[j])
		} else {
			i--
		}
		j++
	}
	if len(ac.filtered) == 0 {
		// no filtered stuff, cancel autocompletion
		return false
	}
	return true
}

func (ac *autocompl) move_cursor_down() {
	if ac.cursor >= len(ac.actual_proposals())-1 {
		return
	}
	ac.cursor++
}

func (ac *autocompl) move_cursor_up() {
	if ac.cursor <= 0 {
		return
	}
	ac.cursor--
}

func (ac *autocompl) desired_height() int {
	proposals := ac.actual_proposals()
	minh := 0
	for i := 0; i < ac_ui_max_lines; i++ {
		n := ac.view + i
		if n >= len(proposals) {
			break
		}
		minh++
	}
	return minh
}

func (ac *autocompl) desired_width(height int) int {
	proposals := ac.actual_proposals()
	minw := 0
	for i := 0; i < height; i++ {
		n := ac.view + i
		line_len := utf8.RuneCount(proposals[n].display)
		if line_len > minw {
			minw = line_len
		}
	}
	return minw + 2
}

func (ac *autocompl) adjust_view(height int) {
	if ac.cursor < ac.view {
		ac.view = ac.cursor
	}

	if ac.cursor >= ac.view+height {
		ac.view = ac.cursor - height + 1
	}
}

func (ac *autocompl) validate_cursor() {
	if ac.cursor >= len(ac.actual_proposals()) {
		ac.cursor = 0
		ac.view = 0
	}
}

// -1 if no need to make a slider
func (ac *autocompl) slider_pos_and_rune(height int) (int, rune) {
	proposals := ac.actual_proposals()
	if len(proposals) == height {
		return -1, 0
	}
	max := len(proposals) - height
	if ac.view == max {
		return height - 1, '▄'
	}

	var r rune
	progress := int((float32(ac.view) / float32(max)) * float32(height*2))
	if progress&1 != 0 {
		r = '▄'
	} else {
		r = '▀'
	}
	return progress / 2, r
}

func (ac *autocompl) draw_onto(buf *tulib.Buffer, x, y int) {
	ac.validate_cursor()

	h := ac.desired_height()
	dst := find_place_for_rect(buf.Rect, tulib.Rect{x, y + 1, 1, h})
	ac.adjust_view(dst.Height)
	w := ac.desired_width(h)
	dst = find_place_for_rect(buf.Rect, tulib.Rect{x, y + 1, w, h})

	slider_i, slider_r := ac.slider_pos_and_rune(dst.Height)
	lp := tulib.DefaultLabelParams

	r := dst
	r.Width--
	r.Height = 1
	for i := 0; i < dst.Height; i++ {
		lp.Fg = termbox.ColorBlack
		lp.Bg = termbox.ColorWhite

		n := ac.view + i
		if n == ac.cursor {
			lp.Fg = termbox.ColorWhite
			lp.Bg = termbox.ColorBlue
		}
		buf.Fill(r, termbox.Cell{
			Fg: lp.Fg,
			Bg: lp.Bg,
			Ch: ' ',
		})
		buf.DrawLabel(r, &lp, ac.actual_proposals()[n].display)

		sr := ' '
		if i == slider_i {
			sr = slider_r
		}
		buf.Set(r.X+r.Width, r.Y, termbox.Cell{
			Fg: termbox.ColorWhite,
			Bg: termbox.ColorBlue,
			Ch: sr,
		})
		r.Y++
	}
}

func (ac *autocompl) finalize(view *view) {
	d := ac.origin.distance(ac.current)
	if d < 0 {
		panic("something went really wrong, oops..")
	}
	data := clone_byte_slice(ac.actual_proposals()[ac.cursor].content[d:])
	view.action_insert(ac.current, data)
	ac.current.boffset += len(data)
	view.move_cursor_to(ac.current)
}

//----------------------------------------------------------------------------
// gocode autocompletion
//----------------------------------------------------------------------------

func gocode_ac_func(path string, r io.Reader, cursor cursor_location_ex) ([]ac_proposal, int) {
	var out bytes.Buffer
	gocode := exec.Command("gocode", "-f=godit",
		"autocomplete", path, strconv.Itoa(cursor.abs_boffset))
	gocode.Stdin = r
	gocode.Stdout = &out

	err := gocode.Run()
	if err != nil {
		return nil, 0
	}

	lr := new_line_reader(out.Bytes())
	charsback_str, proposals_n_str := split_double_csv(lr.read_line())
	charsback, err := atoi(charsback_str)
	if err != nil {
		return nil, 0
	}
	proposals_n, err := atoi(proposals_n_str)
	if err != nil {
		return nil, 0
	}

	proposals := make([]ac_proposal, proposals_n)
	for i := 0; i < proposals_n; i++ {
		d, c := split_double_csv(lr.read_line())
		proposals[i].display = d
		proposals[i].content = c
	}
	return proposals, charsback
}