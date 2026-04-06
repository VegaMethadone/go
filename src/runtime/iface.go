// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

const itabInitSize = 512 // Начальный размер таблицы itab

var (
	itabLock      mutex                               // Мьютекс для синхронизации доступа к таблице
	itabTable     = &itabTableInit                    // Указатель на текущую таблицу интерфейсов
	itabTableInit = itabTableType{size: itabInitSize} // Начальная таблица
)

// Note: change the formula in the mallocgc call in itabAdd if you change these fields.
type itabTableType struct {
	size    uintptr             // Размер массива entries (всегда степень двойки)
	count   uintptr             // Текущее количество заполненных ячеек
	entries [itabInitSize]*itab // Массив указателей на itab (в начальной таблице)
}

// хеш функция
func itabHashFunc(inter *interfacetype, typ *_type) uintptr {
	// compiler has provided some good hash codes for us.
	return uintptr(inter.Type.Hash ^ typ.Hash) // XOR хэшей типа интерфейса и конкретного типа
}

// getitab should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
// функция, которая создает itab ptr для интерфейсного типа и типа
// Функция должна быть внутренней, но некоторые пакеты используют её через linkname
//
//go:linkname getitab
func getitab(inter *interfacetype, typ *_type, canfail bool) *itab {
	// проверяем, что нет методов ( interface{} | any )
	if len(inter.Methods) == 0 {
		throw("internal error - misuse of itab")
	}

	// easy case
	/*
		Проверка того, что у типа нет нужных методов.
		тип не имеет методов, он не может реализовать интерфейс, кроме пустого.
		пустой интерфейс обрабатывается отдельно
	*/
	if typ.TFlag&abi.TFlagUncommon == 0 {
		// если можем зафейлиться - вернемся
		if canfail {
			return nil
		}
		// Иначе паникуем с информацией о типе и интерфейсе
		name := toRType(&inter.Type).nameOff(inter.Methods[0].Name)
		panic(&TypeAssertionError{nil, typ, &inter.Type, name.Name()})
	}

	var m *itab // создаем структуру для хранения интерфейса *abi.Itab

	// First, look in the existing table to see if we can find the itab we need.
	// This is by far the most common case, so do it without locks.
	// Use atomic to ensure we see any previous writes done by the thread
	// that updates the itabTable field (with atomic.Storep in itabAdd).

	// ыбстро сомтрим в таблице без блокировок.
	// атомарно загружаем таблицу itab т.к. она может меняться при ресайзе
	t := (*itabTableType)(atomic.Loadp(unsafe.Pointer(&itabTable)))
	if m = t.find(inter, typ); m != nil {
		// если нашли такую пару
		goto finish
	}

	// Not found.  Grab the lock and try again.
	// Не нашли без блокировки, поэтому берём мьютекс
	lock(&itabLock)
	// Двойная проверка: возможно, другой поток уже создал itab
	if m = itabTable.find(inter, typ); m != nil {
		unlock(&itabLock) // анблок и мы нашли
		goto finish
	}

	// Entry doesn't exist yet. Make a new entry & add it.
	// иначе такого интерфейса еще нет
	// Выделяем память под itab. Размер зависит от количества методов в интерфейсе.
	// Используем persistentalloc, потому что itab живут вечно (не собираются GC)
	m = (*itab)(persistentalloc(unsafe.Sizeof(itab{})+uintptr(len(inter.Methods)-1)*goarch.PtrSize, 0, &memstats.other_sys))

	m.Inter = inter // ассигнмент инерфейсного типа (ptr)
	m.Type = typ    // ассигмент деволтного типа (ptr)

	// The hash is used in type switches. However, compiler statically generates itab's
	// for all interface/type pairs used in switches (which are added to itabTable
	// in itabsinit). The dynamically-generated itab's never participate in type switches,
	// and thus the hash is irrelevant.
	// Note: m.Hash is _not_ the hash used for the runtime itabTable hash table.

	// Хэш не используется для динамически создаваемых itab
	// (компилятор создает itab для switch'ей статически)
	m.Hash = 0

	// Заполняем таблицу методов (связываем методы типа с методами интерфейса)
	// Второй аргумент true означает "только инициализация, без проверки"
	itabInit(m, true)

	// Добавляем новый itab в глобальную таблицу (кэшируем)
	itabAdd(m)

	// Снимаем блокировку
	unlock(&itabLock)
finish:

	// Fun[0] содержит указатель на первую функцию.
	// Если Fun[0] == 0, значит тип не реализует интерфейс.
	if m.Fun[0] != 0 {
		return m
	}

	// Тип не реализует интерфейс
	if canfail {
		return nil // ретернимся
	}
	// this can only happen if the conversion
	// was already done once using the , ok form
	// and we have a cached negative result.
	// The cached result doesn't record which
	// interface function was missing, so initialize
	// the itab again to get the missing function name.

	// Паника с информацией о том, какой метод отсутствует.
	// Вызываем itabInit с false, чтобы получить имя отсутствующего метода.
	panic(&TypeAssertionError{concrete: typ, asserted: &inter.Type, missingMethod: itabInit(m, false)})
}

// find finds the given interface/type pair in t.
// Returns nil if the given interface/type pair isn't present.
// пытаемся найти пару (интерфейс, тип)
func (t *itabTableType) find(inter *interfacetype, typ *_type) *itab {
	// Implemented using quadratic probing.
	// Probe sequence is h(i) = h0 + i*(i+1)/2 mod 2^k.
	// We're guaranteed to hit all table entries using this probe sequence.
	// Используется квадратичное пробирование (quadratic probing)
	// Последовательность пробирования: h(i) = h0 + i*(i+1)/2 mod 2^k
	// Гарантированно проверяем все ячейки таблицы
	mask := t.size - 1
	// берем хеш от (интерфейс, тип) логическое И маска
	h := itabHashFunc(inter, typ) & mask
	for i := uintptr(1); ; i++ {
		// Вычисляем адрес ячейки
		p := (**itab)(add(unsafe.Pointer(&t.entries), h*goarch.PtrSize))
		// Use atomic read here so if we see m != nil, we also see
		// the initializations of the fields of m.
		// m := *p
		// Атомарно загружаем указатель на itab
		m := (*itab)(atomic.Loadp(unsafe.Pointer(p)))
		if m == nil {
			return nil
		}
		// Проверяем, что это нужный нам itab
		if m.Inter == inter && m.Type == typ {
			return m
		}
		// Переходим к следующей ячейке по формуле квадратичного пробирования
		h += i
		h &= mask
	}
}

// itabAdd adds the given itab to the itab hash table.
// itabLock must be held.
// Добавляет заданный itab в хэш-таблицу itab.
// Должен быть удержан itabLock (потокобезопасность).
func itabAdd(m *itab) {
	// Bugs can lead to calling this while mallocing is set,
	// typically because this is called while panicking.
	// Crash reliably, rather than only when we need to grow
	// the hash table.

	// проверка на дедлок
	// т.к. getg().m.mallocing - флаг, что мы уже в процессе выделения памяти
	if getg().m.mallocing != 0 {
		throw("malloc deadlock")
	}

	// текущаю таблица инфрейсов
	t := itabTable
	// если loadFactor больше или равен 75%, мы будем расширятьс
	if t.count >= 3*(t.size/4) { // 75% load factor - стандартное значение для хэш-таблиц
		// При 75% вероятность коллизий становится слишком высокой
		// Grow hash table.
		// t2 = new(itabTableType) + some additional entries
		// We lie and tell malloc we want pointer-free memory because
		// all the pointed-to values are not in the heap.

		// Размер структуры itabTableType:
		// - size (uintptr) + count (uintptr) = 2 * goarch.PtrSize
		// - entries массив: 2*t.size указателей (новая таблица в 2 раза больше)
		// Итого: (2 + 2*t.size) * goarch.PtrSize
		t2 := (*itabTableType)(mallocgc((2+2*t.size)*goarch.PtrSize, nil, true))
		t2.size = t.size * 2

		// Copy over entries.
		// Note: while copying, other threads may look for an itab and
		// fail to find it. That's ok, they will then try to get the itab lock
		// and as a consequence wait until this copying is complete.

		// копирование в новую таблицу старых интерфейсовя
		// p.s. Во время копирования другие потоки могут искать itab и не находить его.
		// Это нормально - они заблокируются на itabLock и будут ждать завершения копирования.
		iterate_itabs(t2.add)
		if t2.count != t.count { // проверка, что было все корретно добавленно
			throw("mismatched count during itab table copy")
		}

		// Publish new hash table. Use an atomic write: see comment in getitab.
		// Атомарная запись гарантирует, что все потоки увидят либо старую, либо новую таблицу целиком
		atomicstorep(unsafe.Pointer(&itabTable), unsafe.Pointer(t2))
		// Adopt the new table as our own.
		// присваение нового указателя
		t = itabTable
		// Note: the old table can be GC'ed here.
		// GC почистит старую таблицу
	}
	// добавляем новый инфрейс, ура
	t.add(m)
}

// add adds the given itab to itab table t.
// itabLock must be held.
// просто добавляем *itab в новую таблицу, ничего сложного
// работает только при StopTheWorld
func (t *itabTableType) add(m *itab) {
	// See comment in find about the probe sequence.
	// Insert new itab in the first empty spot in the probe sequence.
	mask := t.size - 1
	h := itabHashFunc(m.Inter, m.Type) & mask
	for i := uintptr(1); ; i++ {
		p := (**itab)(add(unsafe.Pointer(&t.entries), h*goarch.PtrSize))
		m2 := *p
		// проверка, что такой itab уже есть
		if m2 == m {
			// A given itab may be used in more than one module
			// and thanks to the way global symbol resolution works, the
			// pointed-to itab may already have been inserted into the
			// global 'hash'.
			return
		}

		// если упстая ячейка
		if m2 == nil {
			// Use atomic write here so if a reader sees m, it also
			// sees the correctly initialized fields of m.
			// NoWB is ok because m is not in heap memory.
			// *p = m
			// Атомарная запись
			atomic.StorepNoWB(unsafe.Pointer(p), unsafe.Pointer(m))
			t.count++
			return
		}
		// Коллизия: ячейка занята другим itab, пробуем следующую
		h += i
		h &= mask
	}
}

// itabInit fills in the m.Fun array with all the code pointers for
// the m.Inter/m.Type pair. If the type does not implement the interface,
// it sets m.Fun[0] to 0 and returns the name of an interface function that is missing.
// If !firstTime, itabInit will not write anything to m.Fun (see issue 65962).
// It is ok to call this multiple times on the same m, even concurrently
// (although it will only be called once with firstTime==true).
func itabInit(m *itab, firstTime bool) string { // инициализация

	// подготовка
	inter := m.Inter    // интерфейсный тип
	typ := m.Type       // конкретный тип
	x := typ.Uncommon() // указатель на дату, иначе nil ??

	// both inter and typ have method sorted by name,
	// and interface names are unique,
	// so can iterate over both in lock step;
	// the loop is O(ni+nt) not O(ni*nt).

	ni := len(inter.Methods) // Количество методов в интерфейсе
	nt := int(x.Mcount)      // Количество методов в типе

	// Получение списка методов типа co смещение от начала uncommonType до массива методов
	xmhdr := (*[1 << 16]abi.Method)(add(unsafe.Pointer(x), uintptr(x.Moff)))[:nt:nt]
	j := 0 // Указатель на методы типа

	// Подготовка массива для указателей на функции
	methods := (*[1 << 16]unsafe.Pointer)(unsafe.Pointer(&m.Fun[0]))[:ni:ni]
	var fun0 unsafe.Pointer // Указатель на первую функцию
imethods:
	// метод двух указателей
	for k := 0; k < ni; k++ {

		i := &inter.Methods[k] // Текущий метод интерфейса

		// Получаем информацию о методе интерфейса
		itype := toRType(&inter.Type).typeOff(i.Typ) // Сигнатура метода
		name := toRType(&inter.Type).nameOff(i.Name) // Имя метода
		iname := name.Name()                         // Имя как строка
		ipkg := pkgPath(name)                        // Пакет метода
		if ipkg == "" {
			ipkg = inter.PkgPath.Name() // Пакет интерфейса по умолчанию
		}

		// Ищем соответствующий метод в типе
		for ; j < nt; j++ {
			t := &xmhdr[j]
			rtyp := toRType(typ)
			tname := rtyp.nameOff(t.Name)
			// сравниваем сигнатуры методов (типы) и индефикаторы методов
			if rtyp.typeOff(t.Mtyp) == itype && tname.Name() == iname {
				// Проверяем видимость (экспортирован или в том же пакете)
				pkgPath := pkgPath(tname)
				if pkgPath == "" {
					pkgPath = rtyp.nameOff(x.PkgPath).Name()
				}
				if tname.IsExported() || pkgPath == ipkg {
					// нашли, получаем указатель на функцию
					ifn := rtyp.textOff(t.Ifn)
					if k == 0 {
						fun0 = ifn // Сохраняем первую функцию отдельно
					} else if firstTime {
						methods[k] = ifn // Записываем указатель
					}

					continue imethods // Переходим к следующему методу интерфейса
				}
			}
		}
		// Не нашли метод - тип не реализует интерфейс
		// Возвращаем имя отсутствующего метода
		return iname
	}
	if firstTime {
		m.Fun[0] = uintptr(fun0)
	}
	return ""
}

// itabsinit инициализирует глобальную таблицу itab,
// добавляя все предсозданные itab'ы из активных модулей.
func itabsinit() {
	lockInit(&itabLock, lockRankItab)    // инициализируем мьютекс
	lock(&itabLock)                      // лочимся
	for _, md := range activeModules() { // возвращает все модули программы
		for _, i := range md.itablinks {
			itabAdd(i) // Добавляем каждый itab в глобальную таблицу
		}
	}
	unlock(&itabLock) // анлок
}

// В го есть 2 типа тайп ассершена.
// 1. С паникой value := x.(Type)
// 2. Без паники value, ok := x.(Type)

// Вызывается при неудачной конвертации e.(T) где e - interface{}
// have = динамический тип, который есть у значения
// want = статический тип, к которому пытаемся преобразовать
// iface = статический тип, из которого преобразуем (interface{})
/*
	var x interface{} = "hello"
	var y int = x.(int)  // PANIC! И вызовет panicdottypeE
                     	 // have = string тип
                     	 // want = int тип
                     	 // iface = interface{} тип
*/
func panicdottypeE(have, want, iface *_type) {
	panic(&TypeAssertionError{iface, have, want, ""})
}

// panicdottypeI вызывается при неудачном преобразовании i.(T)
// Параметры как в panicdottypeE, но "have" - это itab, который у нас уже есть
func panicdottypeI(have *itab, want, iface *_type) {
	var t *_type
	if have != nil {
		t = have.Type // Извлекаем тип из itab
	}
	panicdottypeE(t, want, iface) // Делегируем
}

// Вызывается при конвертации i.(T) когда интерфейс i равен nil
// want = статический тип, к которому пытаемся преобразовать
/*
	var i io.Reader  // nil интерфейс
	var r *os.File = i.(*os.File)   // PANIC! Вызовет panicnildottype
									// i = nil
									// want = *os.File тип
*/
func panicnildottype(want *_type) {
	panic(&TypeAssertionError{nil, nil, want, ""})
	// TODO: Add the static type we're converting from as well.
	// It might generate a better error message.
	// Just to match other nil conversion errors, we don't for now.
}

// The specialized convTx routines need a type descriptor to use when calling mallocgc.
// We don't need the type to be exact, just to have the correct size, alignment, and pointer-ness.
// However, when debugging, it'd be nice to have some indication in mallocgc where the types came from,
// so we use named types here.
// We then construct interface values of these types,
// and then extract the type word to use as needed.
type (
	uint16InterfacePtr uint16
	uint32InterfacePtr uint32
	uint64InterfacePtr uint64
	stringInterfacePtr string
	sliceInterfacePtr  []byte
)

var (
	uint16Eface any = uint16InterfacePtr(0)
	uint32Eface any = uint32InterfacePtr(0)
	uint64Eface any = uint64InterfacePtr(0)
	stringEface any = stringInterfacePtr("")
	sliceEface  any = sliceInterfacePtr(nil)

	uint16Type *_type = efaceOf(&uint16Eface)._type
	uint32Type *_type = efaceOf(&uint32Eface)._type
	uint64Type *_type = efaceOf(&uint64Eface)._type
	stringType *_type = efaceOf(&stringEface)._type
	sliceType  *_type = efaceOf(&sliceEface)._type
)

// The conv and assert functions below do very similar things.
// The convXXX functions are guaranteed by the compiler to succeed.
// The assertXXX functions may fail (either panicking or returning false,
// depending on whether they are 1-result or 2-result).
// The convXXX functions succeed on a nil input, whereas the assertXXX
// functions fail on a nil input.

// convT преобразует значение типа t (на которое указывает v) в указатель,
// который может быть использован как второе слово интерфейсного значения.
// Скопировать значение из стека (или где оно сейчас) в кучу (heap), чтобы интерфейс мог на него ссылаться.
func convT(t *_type, v unsafe.Pointer) unsafe.Pointer {
	// проверка, если race condition decttor включен
	if raceenabled {
		// raceReadObjectPC отмечает чтение объекта для детектора гонок:
		// - t: тип объекта (для понимания размера)
		// - v: адрес объекта
		// - sys.GetCallerPC(): адрес возврата из функции, которая вызвала convT
		// - abi.FuncPCABIInternal(convT): адрес самой функции convT
		// Это нужно, чтобы детектор гонок знал, что мы читаем значение по адресу v
		raceReadObjectPC(t, v, sys.GetCallerPC(), abi.FuncPCABIInternal(convT))
	}
	// Проверка включенного MemorySanitizer (обнаруживает чтение неинициализированной памяти)
	if msanenabled {
		// msanread сообщает MemorySanitizer, что мы читаем t.Size_ байт из v
		// Это помогает обнаружить использование неинициализированной памяти
		// p.s. не вызовется
		msanread(v, t.Size_)
	}
	// Проверка включенного AddressSanitizer (обнаруживает ошибки адресации памяти)
	if asanenabled {
		// asanread проверяет, что доступ к памяти по адресу v длиной t.Size_ безопасен
		// Обнаруживает обращения за границы буфера, use-after-free и т.д.
		// p.s. не вызовется
		asanread(v, t.Size_)
	}

	// Выделение памяти в управляемой куче (GC heap):
	// - t.Size_: количество байт, которое нужно выделить (размер типа)
	// - t: информация о типе (нужна сборщику мусора для сканирования указателей внутри типа)
	// - true: обнулить выделенную память (все байты = 0)
	// x - указатель на новую выделенную область памяти в куче
	x := mallocgc(t.Size_, t, true)

	// копирование из исходного места в новую память (from V to X)
	typedmemmove(t, x, v)
	return x
}

func convTnoptr(t *_type, v unsafe.Pointer) unsafe.Pointer {
	// TODO: maybe take size instead of type?
	if raceenabled {
		raceReadObjectPC(t, v, sys.GetCallerPC(), abi.FuncPCABIInternal(convTnoptr))
	}
	if msanenabled {
		msanread(v, t.Size_)
	}
	if asanenabled {
		asanread(v, t.Size_)
	}

	x := mallocgc(t.Size_, t, false)
	memmove(x, v, t.Size_)
	return x
}

func convT16(val uint16) (x unsafe.Pointer) {
	if val < uint16(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
		if goarch.BigEndian {
			x = add(x, 6)
		}
	} else {
		x = mallocgc(2, uint16Type, false)
		*(*uint16)(x) = val
	}
	return
}

func convT32(val uint32) (x unsafe.Pointer) {
	if val < uint32(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
		if goarch.BigEndian {
			x = add(x, 4)
		}
	} else {
		x = mallocgc(4, uint32Type, false)
		*(*uint32)(x) = val
	}
	return
}

// convT64 should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname convT64
func convT64(val uint64) (x unsafe.Pointer) {
	if val < uint64(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
	} else {
		x = mallocgc(8, uint64Type, false)
		*(*uint64)(x) = val
	}
	return
}

// convTstring should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname convTstring
func convTstring(val string) (x unsafe.Pointer) {
	if val == "" {
		x = unsafe.Pointer(&zeroVal[0])
	} else {
		x = mallocgc(unsafe.Sizeof(val), stringType, true)
		*(*string)(x) = val
	}
	return
}

// convTslice should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname convTslice
func convTslice(val []byte) (x unsafe.Pointer) {
	// Note: this must work for any element type, not just byte.
	if (*slice)(unsafe.Pointer(&val)).array == nil {
		x = unsafe.Pointer(&zeroVal[0])
	} else {
		x = mallocgc(unsafe.Sizeof(val), sliceType, true)
		*(*[]byte)(x) = val
	}
	return
}

// преобразование пустого интерфейса в непустой
// var x any
// var r error = x.(error) // panic assertE2I
func assertE2I(inter *interfacetype, t *_type) *itab {
	if t == nil {
		// Явные преобразования требуют ненулевое значение интерфейса
		panic(&TypeAssertionError{nil, nil, &inter.Type, ""})
	}
	return getitab(inter, t, false) // false = паниковать если не удалось
}

// Используется для преобразования: val, ok := x.(Интерфейс)
// Пример: var x interface{} = os.Stdin; r, ok := x.(io.Reader)
// Если преобразование невозможно → возвращает nil (ok = false)
func assertE2I2(inter *interfacetype, t *_type) *itab {
	if t == nil {
		return nil // Просто возвращаем nil, без паники
	}
	return getitab(inter, t, true) // true = не паниковать, вернуть nil если не удалось
}

// typeAssert строит itab для конкретного типа t и интерфейса s.Inter.
// Если преобразование невозможно, паникует при s.CanFail=false и возвращает nil при s.CanFail=true.
func typeAssert(s *abi.TypeAssert, t *_type) *itab {
	var tab *itab // инициализируем интерфейс
	// Если тип t == nil, значит исходное значение было nil
	if t == nil {
		if !s.CanFail {
			// s.CanFail = false → это конструкция с одной переменной (паникует)
			// Пример: var x interface{} = nil; y := x.(SomeType)
			panic(&TypeAssertionError{nil, nil, &s.Inter.Type, ""})
		}
		// s.CanFail = true → конструкция с двумя переменными (не паникует)
		// Пример: var x interface{} = nil; y, ok := x.(SomeType)
		// Просто возвращаем nil, во внешнем коде ok будет false
	} else {
		// t != nil - есть реальный тип, получаем itab
		// getitab ищет в таблице или создает новый itab для пары (интерфейс, тип)
		// s.CanFail передается дальше:
		// - false → getitab вызовет панику если тип не реализует интерфейс
		// - true → getitab вернет nil если тип не реализует интерфейс
		tab = getitab(s.Inter, t, s.CanFail)
	}

	// ахахахах
	// На некоторых старых/специальных процессорах эта оптимизация не работает. Нахуй кэш, возвращаем как есть. НАХУЙЙЙЙЙЙЙЙЙЙЙЙЙЙ
	if !abi.UseInterfaceSwitchCache(goarch.ArchFamily) {
		return tab
	}

	// Maybe update the cache, so the next time the generated code
	// doesn't need to call into the runtime.
	// cheaprand() - быстрый генератор псевдослучайных чисел
	// & 1023 проверяет последние 10 бит (0-1023)
	if cheaprand()&1023 != 0 {
		// Only bother updating the cache ~1 in 1000 times.
		// В ~99.9% случаев не обновляем кэш, чтобы не тратить время
		return tab
	}

	// Load the current cache.
	oldC := (*abi.TypeAssertCache)(atomic.Loadp(unsafe.Pointer(&s.Cache)))

	// oldC.Mask = размер_кэша - 1
	// Чем больше кэш, тем реже обновляем (амортизация затрат)
	if cheaprand()&uint32(oldC.Mask) != 0 {
		// As cache gets larger, choose to update it less often
		// so we can amortize the cost of building a new cache.
		return tab
	}

	// Make a new cache.
	newC := buildTypeAssertCache(oldC, t, tab)

	// Update cache. Use compare-and-swap so if multiple threads
	// are fighting to update the cache, at least one of their
	// updates will stick.
	// Если s.Cache все еще равен oldC, заменяем на newC
	// Если другой поток уже обновил кэш, наша операция фейлится (но это нормально)
	atomic_casPointer((*unsafe.Pointer)(unsafe.Pointer(&s.Cache)), unsafe.Pointer(oldC), unsafe.Pointer(newC))

	return tab
}

func buildTypeAssertCache(oldC *abi.TypeAssertCache, typ *_type, tab *itab) *abi.TypeAssertCache { // потом, заебался
	oldEntries := unsafe.Slice(&oldC.Entries[0], oldC.Mask+1)

	// Count the number of entries we need.
	n := 1
	for _, e := range oldEntries {
		if e.Typ != 0 {
			n++
		}
	}

	// Figure out how big a table we need.
	// We need at least one more slot than the number of entries
	// so that we are guaranteed an empty slot (for termination).
	newN := n * 2                         // make it at most 50% full
	newN = 1 << sys.Len64(uint64(newN-1)) // round up to a power of 2

	// Allocate the new table.
	newSize := unsafe.Sizeof(abi.TypeAssertCache{}) + uintptr(newN-1)*unsafe.Sizeof(abi.TypeAssertCacheEntry{})
	newC := (*abi.TypeAssertCache)(mallocgc(newSize, nil, true))
	newC.Mask = uintptr(newN - 1)
	newEntries := unsafe.Slice(&newC.Entries[0], newN)

	// Fill the new table.
	addEntry := func(typ *_type, tab *itab) {
		h := int(typ.Hash) & (newN - 1)
		for {
			if newEntries[h].Typ == 0 {
				newEntries[h].Typ = uintptr(unsafe.Pointer(typ))
				newEntries[h].Itab = uintptr(unsafe.Pointer(tab))
				return
			}
			h = (h + 1) & (newN - 1)
		}
	}
	for _, e := range oldEntries {
		if e.Typ != 0 {
			addEntry((*_type)(unsafe.Pointer(e.Typ)), (*itab)(unsafe.Pointer(e.Itab)))
		}
	}
	addEntry(typ, tab)

	return newC
}

// Empty type assert cache. Contains one entry with a nil Typ (which
// causes a cache lookup to fail immediately.)
var emptyTypeAssertCache = abi.TypeAssertCache{Mask: 0}

// interfaceSwitch compares t against the list of cases in s.
// If t matches case i, interfaceSwitch returns the case index i and
// an itab for the pair <t, s.Cases[i]>.
// If there is no match, return N,nil, where N is the number
// of cases.
func interfaceSwitch(s *abi.InterfaceSwitch, t *_type) (int, *itab) { // потом, заебался
	cases := unsafe.Slice(&s.Cases[0], s.NCases)

	// Results if we don't find a match.
	case_ := len(cases)
	var tab *itab

	// Look through each case in order.
	for i, c := range cases {
		tab = getitab(c, t, true)
		if tab != nil {
			case_ = i
			break
		}
	}

	if !abi.UseInterfaceSwitchCache(goarch.ArchFamily) {
		return case_, tab
	}

	// Maybe update the cache, so the next time the generated code
	// doesn't need to call into the runtime.
	if cheaprand()&1023 != 0 {
		// Only bother updating the cache ~1 in 1000 times.
		// This ensures we don't waste memory on switches, or
		// switch arguments, that only happen a few times.
		return case_, tab
	}
	// Load the current cache.
	oldC := (*abi.InterfaceSwitchCache)(atomic.Loadp(unsafe.Pointer(&s.Cache)))

	if cheaprand()&uint32(oldC.Mask) != 0 {
		// As cache gets larger, choose to update it less often
		// so we can amortize the cost of building a new cache
		// (that cost is linear in oldc.Mask).
		return case_, tab
	}

	// Make a new cache.
	newC := buildInterfaceSwitchCache(oldC, t, case_, tab)

	// Update cache. Use compare-and-swap so if multiple threads
	// are fighting to update the cache, at least one of their
	// updates will stick.
	atomic_casPointer((*unsafe.Pointer)(unsafe.Pointer(&s.Cache)), unsafe.Pointer(oldC), unsafe.Pointer(newC))

	return case_, tab
}

// buildInterfaceSwitchCache constructs an interface switch cache
// containing all the entries from oldC plus the new entry
// (typ,case_,tab).
func buildInterfaceSwitchCache(oldC *abi.InterfaceSwitchCache, typ *_type, case_ int, tab *itab) *abi.InterfaceSwitchCache {
	oldEntries := unsafe.Slice(&oldC.Entries[0], oldC.Mask+1)

	// Count the number of entries we need.
	n := 1
	for _, e := range oldEntries {
		if e.Typ != 0 {
			n++
		}
	}

	// Figure out how big a table we need.
	// We need at least one more slot than the number of entries
	// so that we are guaranteed an empty slot (for termination).
	newN := n * 2                         // make it at most 50% full
	newN = 1 << sys.Len64(uint64(newN-1)) // round up to a power of 2

	// Allocate the new table.
	newSize := unsafe.Sizeof(abi.InterfaceSwitchCache{}) + uintptr(newN-1)*unsafe.Sizeof(abi.InterfaceSwitchCacheEntry{})
	newC := (*abi.InterfaceSwitchCache)(mallocgc(newSize, nil, true))
	newC.Mask = uintptr(newN - 1)
	newEntries := unsafe.Slice(&newC.Entries[0], newN)

	// Fill the new table.
	addEntry := func(typ *_type, case_ int, tab *itab) {
		h := int(typ.Hash) & (newN - 1)
		for {
			if newEntries[h].Typ == 0 {
				newEntries[h].Typ = uintptr(unsafe.Pointer(typ))
				newEntries[h].Case = case_
				newEntries[h].Itab = uintptr(unsafe.Pointer(tab))
				return
			}
			h = (h + 1) & (newN - 1)
		}
	}
	for _, e := range oldEntries {
		if e.Typ != 0 {
			addEntry((*_type)(unsafe.Pointer(e.Typ)), e.Case, (*itab)(unsafe.Pointer(e.Itab)))
		}
	}
	addEntry(typ, case_, tab)

	return newC
}

// Empty interface switch cache. Contains one entry with a nil Typ (which
// causes a cache lookup to fail immediately.)
var emptyInterfaceSwitchCache = abi.InterfaceSwitchCache{Mask: 0}

// reflect_ifaceE2I is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/modern-go/reflect2
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
//
//go:linkname reflect_ifaceE2I reflect.ifaceE2I
func reflect_ifaceE2I(inter *interfacetype, e eface, dst *iface) {
	*dst = iface{assertE2I(inter, e._type), e.data}
}

//go:linkname reflectlite_ifaceE2I internal/reflectlite.ifaceE2I
func reflectlite_ifaceE2I(inter *interfacetype, e eface, dst *iface) {
	*dst = iface{assertE2I(inter, e._type), e.data}
}

// удобно, стоп зе ворлд конечно - грустно. Потому что глобал значение и должно быть синхронизованно
func iterate_itabs(fn func(*itab)) {
	// Note: only runs during stop the world or with itabLock held,
	// so no other locks/atomics needed.
	t := itabTable
	for i := uintptr(0); i < t.size; i++ {
		m := *(**itab)(add(unsafe.Pointer(&t.entries), i*goarch.PtrSize))
		if m != nil {
			fn(m)
		}
	}
}

// staticuint64s is used to avoid allocating in convTx for small integer values.
// staticuint64s[0] == 0, staticuint64s[1] == 1, and so forth.
// It is defined in assembler code so that it is read-only.
var staticuint64s [256]uint64

// getStaticuint64s is called by the reflect package to get a pointer
// to the read-only array.
//
//go:linkname getStaticuint64s
func getStaticuint64s() *[256]uint64 {
	return &staticuint64s
}

// The linker redirects a reference of a method that it determined
// unreachable to a reference to this function, so it will throw if
// ever called.
func unreachableMethod() {
	throw("unreachable method called. linker bug?")
}
