package main

import "testing"

func paramField(t *testing.T, p any, field string) any {
	t.Helper()
	m, ok := p.(*OMap)
	if !ok {
		t.Fatalf("параметр не *OMap, а %T", p)
	}
	v, _ := m.Get(field)
	return v
}

func TestRequiredHeadersGoFirst(t *testing.T) {
	existing := NewM().Set("name", "orderId").Set("in", "path")
	got := withRequiredHeaders([]any{existing}, "12345")

	if len(got) != 3 {
		t.Fatalf("параметров %d, ожидалось 3", len(got))
	}
	want := []struct{ name, example string }{
		{authTokenHeader, authTokenExample},
		{merchantUIDHeader, "12345"},
	}
	for i, w := range want {
		if n := paramField(t, got[i], "name"); n != w.name {
			t.Errorf("параметр %d: %v, ожидался %s", i, n, w.name)
		}
		if in := paramField(t, got[i], "in"); in != "header" {
			t.Errorf("%s: in = %v, ожидалось header", w.name, in)
		}
		if req := paramField(t, got[i], "required"); req != true {
			t.Errorf("%s: required = %v, заголовок обязателен", w.name, req)
		}
		if ex := paramField(t, got[i], "example"); ex != w.example {
			t.Errorf("%s: example = %v, ожидалось %s", w.name, ex, w.example)
		}
	}
	if n := paramField(t, got[2], "name"); n != "orderId" {
		t.Errorf("исходный параметр потерялся: %v", n)
	}
}

func TestMerchantUIDFallsBackToVariable(t *testing.T) {
	// Пустой обязательный заголовок хуже отсутствующего: он перекроет
	// значение, заданное на уровне коллекции.
	got := withRequiredHeaders(nil, "")
	for _, p := range got {
		if paramField(t, p, "name") == merchantUIDHeader {
			if ex := paramField(t, p, "example"); ex != merchantUIDExample {
				t.Errorf("example = %v, ожидалась подстановка %s", ex, merchantUIDExample)
			}
			return
		}
	}
	t.Fatalf("заголовок %s не найден", merchantUIDHeader)
}

func TestRequiredHeadersDoNotDuplicate(t *testing.T) {
	// Если заголовки появятся в примерах Kaspi Гид — в любом регистре.
	doc := []any{
		NewM().Set("name", "x-merchant-uid").Set("in", "header").Set("required", false),
		NewM().Set("name", "X-AUTH-TOKEN").Set("in", "header").Set("required", false),
	}
	got := withRequiredHeaders(doc, "12345")

	if len(got) != 2 {
		t.Fatalf("параметров %d, ожидалось 2 — заголовки задвоились", len(got))
	}
	for _, p := range got {
		if req := paramField(t, p, "required"); req != true {
			t.Errorf("%v: required = %v, ожидалось true", paramField(t, p, "name"), req)
		}
	}
}

func TestBuildSpecPutsHeaderInEveryOperation(t *testing.T) {
	items := []*Item{
		{
			Section: "orders", QID: "q1", Title: "Как получить заказы?",
			URL: "https://guide.kaspi.kz/partner/ru/shop/api/orders/q1",
			Examples: []Example{{
				Title: "Пример запроса",
				Text:  "GET https://kaspi.kz/shop/api/v2/orders?page[number]=0\nX-Auth-Token: token",
			}},
		},
		{
			Section: "goods", QID: "q2", Title: "Как изменить цену?",
			URL: "https://guide.kaspi.kz/partner/ru/shop/api/goods/q2",
			Examples: []Example{{
				Title: "Пример запроса",
				Text:  "POST https://kaspi.kz/shop/api/products/import\nContent-Type: application/json\n\n{\"sku\":\"1\"}",
			}},
		},
	}

	spec := buildSpec(items, "42")
	pathsAny, ok := spec.Get("paths")
	if !ok {
		t.Fatal("в схеме нет paths")
	}
	paths := pathsAny.(*OMap)
	if paths.Len() == 0 {
		t.Fatal("ни одной операции не собралось")
	}
	for _, pk := range paths.keys {
		methodsAny, _ := paths.Get(pk)
		methods := methodsAny.(*OMap)
		for _, mk := range methods.keys {
			opAny, _ := methods.Get(mk)
			params, ok := opAny.(*OMap).Get("parameters")
			if !ok {
				t.Errorf("%s %s: нет параметров вовсе", mk, pk)
				continue
			}
			found := map[string]bool{}
			for _, p := range params.([]any) {
				switch paramField(t, p, "name") {
				case merchantUIDHeader:
					found[merchantUIDHeader] = true
					if ex := paramField(t, p, "example"); ex != "42" {
						t.Errorf("%s %s: example = %v, ожидалось 42", mk, pk, ex)
					}
				case authTokenHeader:
					found[authTokenHeader] = true
					if ex := paramField(t, p, "example"); ex != authTokenExample {
						t.Errorf("%s %s: example = %v, ожидалось %s", mk, pk, ex, authTokenExample)
					}
				}
			}
			for _, h := range []string{authTokenHeader, merchantUIDHeader} {
				if !found[h] {
					t.Errorf("%s %s: нет заголовка %s", mk, pk, h)
				}
			}
		}
	}
}

func TestBuildSpecHasNoSecuritySection(t *testing.T) {
	// Токен теперь обычный заголовок, схема безопасности только мешала бы:
	// клиенты добавляли бы тот же заголовок второй раз, с другим значением.
	spec := buildSpec(nil, "42")
	if _, ok := spec.Get("security"); ok {
		t.Error("в схеме остался раздел security")
	}
	if c, ok := spec.Get("components"); ok {
		if _, ok := c.(*OMap).Get("securitySchemes"); ok {
			t.Error("в схеме остались securitySchemes")
		}
	}
}
