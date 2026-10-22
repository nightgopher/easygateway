package console

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

type listViewState struct {
	models     rowFilter[Model]
	logs       rowFilter[Log]
	skills     rowFilter[Skill]
	modelList  ui.ListState
	skillList  ui.ListState
	marketList ui.ListState
}

// rowFilter keeps only one search result. API reloads replace the source slice;
// rows must not be edited in place while cached. Matching text is built lazily
// for searches, never for an unfiltered list or on every view update.
type rowFilter[T any] struct {
	source      []T
	input       string
	query       string
	initialized bool
	searchText  []string
	matches     []int
}

type visibleRows[T any] struct {
	source   []T
	indices  []int
	filtered bool
}

func (v visibleRows[T]) Len() int {
	if v.filtered {
		return len(v.indices)
	}
	return len(v.source)
}

func (v visibleRows[T]) At(i int) T {
	if v.filtered {
		i = v.indices[i]
	}
	return v.source[i]
}

func sameRows[T any](a, b []T) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (f *rowFilter[T]) apply(source []T, query string, text func(T) string) (visibleRows[T], bool) {
	if !f.initialized || query != f.input {
		f.input = query
		query = strings.ToLower(query)
	} else {
		query = f.query
	}
	sourceChanged := !sameRows(f.source, source)
	changed := !f.initialized || sourceChanged || f.query != query
	if changed {
		if sourceChanged {
			f.source = source
			f.searchText = nil
			f.matches = nil
		}
		f.initialized, f.query = true, query
		if query != "" {
			if len(f.searchText) != len(source) {
				f.searchText = make([]string, len(source))
				for i, row := range source {
					f.searchText[i] = strings.ToLower(text(row))
				}
			}
			if cap(f.matches) < len(source) {
				f.matches = make([]int, 0, len(source))
			} else {
				f.matches = f.matches[:0]
			}
			for i, row := range f.searchText {
				if strings.Contains(row, query) {
					f.matches = append(f.matches, i)
				}
			}
		}
	}
	return visibleRows[T]{source: source, indices: f.matches, filtered: query != ""}, changed
}

func modelSearchText(m Model) string { return m.Name + " " + m.Provider }

// modelCategoryAll 是能力分类的“不过滤”哨兵选项。
const modelCategoryAll = "全部能力"

// modelCategories 是模型页的能力分类，值映射到网关能力名；
// 向量与语音转写模型在目录里与对话模型并列，靠分类区分用途。
var modelCategories = []string{modelCategoryAll, "文本", "视觉", "视频", "向量", "语音转写", "结构化判断", "推理", "工具调用"}

// modelInCategory 报告模型是否属于所选能力分类。
func modelInCategory(m Model, category string) bool {
	if category == "" || category == modelCategoryAll {
		return true
	}
	capability := map[string]string{
		"文本": "text", "视觉": "vision", "视频": "video",
		"向量": "embedding", "语音转写": "audio_transcription",
		"结构化判断": "systemone",
		"推理":    "reasoning", "工具调用": "tool_calling",
	}[category]
	if capability == "" {
		return true
	}
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

func skillSearchText(s Skill) string {
	return s.ID + " " + s.Name + " " + s.Emoji + " " + s.Description + " " + s.Author + " " + strings.Join(s.Tags, " ")
}

func logSearchText(l Log) string {
	return l.Model + " " + l.Provider + " " + l.RequestID + " " + l.Error
}
