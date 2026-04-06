// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancellation signals, and other request-scoped values across API boundaries
// and between processes.
//
// Incoming requests to a server should create a [Context], and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using [WithCancel], [WithDeadline],
// [WithTimeout], or [WithValue].
//
// A Context may be canceled to indicate that work done on its behalf should stop.
// A Context with a deadline is canceled after the deadline passes.
// When a Context is canceled, all Contexts derived from it are also canceled.
//
// The [WithCancel], [WithDeadline], and [WithTimeout] functions take a
// Context (the parent) and return a derived Context (the child) and a
// [CancelFunc]. Calling the CancelFunc directly cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled. The go vet tool
// checks that CancelFuncs are used on all control-flow paths.
//
// The [WithCancelCause], [WithDeadlineCause], and [WithTimeoutCause] functions
// return a [CancelCauseFunc], which takes an error and records it as
// the cancellation cause. Calling [Cause] on the canceled context
// or any of its children retrieves the cause. If no cause is specified,
// Cause(ctx) returns the same value as ctx.Err().
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. This is discussed further in
// https://go.dev/blog/context-and-structs. The Context should be the first
// parameter, typically named ctx:
//
//	func DoSomething(ctx context.Context, arg Arg) error {
//		// ... use ctx ...
//	}
//
// Do not pass a nil [Context], even if a function permits it. Pass [context.TODO]
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
//
// See https://go.dev/blog/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// A Context carries a deadline, a cancellation signal, and other values across
// API boundaries.
//
// Context's methods may be called by multiple goroutines simultaneously.
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled. Deadline returns ok==false when no deadline is
	// set. Successive calls to Deadline return the same results.

	// Определяет время жизни нашего канала.
	// Вернем ok==false в том случае, когда  дедлайна не было.
	// возвращает время, когда работа должна быть отменена
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled. Done may return nil if this context can
	// never be canceled. Successive calls to Done return the same value.
	// The close of the Done channel may happen asynchronously,
	// after the cancel function returns.
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	//  // Stream generates values with DoSomething and sends them to out
	//  // until DoSomething returns an error or ctx.Done is closed.
	//  func Stream(ctx context.Context, out chan<- Value) error {
	//  	for {
	//  		v, err := DoSomething(ctx)
	//  		if err != nil {
	//  			return err
	//  		}
	//  		select {
	//  		case <-ctx.Done():
	//  			return ctx.Err()
	//  		case out <- v:
	//  		}
	//  	}
	//  }
	//
	// See https://blog.golang.org/pipelines for more examples of how to use
	// a Done channel for cancellation.

	// возвращает канал, закрываемый при отмене контекста
	Done() <-chan struct{}

	// If Done is not yet closed, Err returns nil.
	// If Done is closed, Err returns a non-nil error explaining why:
	// DeadlineExceeded if the context's deadline passed,
	// or Canceled if the context was canceled for some other reason.
	// After Err returns a non-nil error, successive calls to Err return the same error.

	// возвращает ошибку отмены (либо nil, если контекст активен)
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key. Successive calls to Value with
	// the same key returns the same result.
	//
	// Use context values only for request-scoped data that transits
	// processes and API boundaries, not for passing optional parameters to
	// functions.
	//
	// A key identifies a specific value in a Context. Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value. A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stored using that key:
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts. It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	// возвращает значение, связанное с ключом
	Value(key any) any
}

// Canceled - глобальная переменная ошибки, возвращаемая Context.Err()
// когда контекст отменен по причине, отличной от истечения дедлайна
// (например, при явном вызове cancel() или отмене родительского контекста)
var Canceled = errors.New("context canceled")

// DeadlineExceeded - глобальная переменная ошибки, возвращаемая Context.Err()
// когда контекст отменен из-за истечения установленного дедлайна
var DeadlineExceeded error = deadlineExceededError{}

// deadlineExceededError - внутренний тип, реализующий интерфейс error
// с дополнительными методами Timeout() и Temporary()
type deadlineExceededError struct{}

// Error() возвращает текстовое описание ошибки
func (deadlineExceededError) Error() string { return "context deadline exceeded" }

// Timeout() возвращает true, указывая что это ошибка таймаута
// Это полезно для проверки типа ошибки (пакет net использует это)

func (deadlineExceededError) Timeout() bool { return true }

// Temporary() возвращает true, указывая что это временная ошибка
// (можно попробовать операцию снова)
func (deadlineExceededError) Temporary() bool { return true }

// emptyCtx - базовая реализация контекста, которая никогда не отменяется,
// не имеет значений и дедлайна. Является основой для backgroundCtx и todoCtx
// (Этот тип используется для Background() и TODO() контекстов)
type emptyCtx struct{}

// Deadline() возвращает нулевое время и false, так как у emptyCtx нет дедлайна
func (emptyCtx) Deadline() (deadline time.Time, ok bool) {
	// p.s. если в тьюпле указаные индефикаторы
	// то го их проинициализирует дефолтными значениями и вернет без явного указания в return pog
	return
}

// Done() возвращает nil, так как emptyCtx никогда не отменяется
// Канал, который никогда не закрывается - операции с ним будут вечно ждать
func (emptyCtx) Done() <-chan struct{} {
	return nil
}

// Err() всегда возвращает nil, так как контекст никогда не отменяется
func (emptyCtx) Err() error {
	return nil
}

// Value() всегда возвращает nil, так как emptyCtx не хранит значений
func (emptyCtx) Value(key any) any {
	return nil
}

// backgroundCtx - структура для корневого контекста
// Встраивает emptyCtx, получая все его методы (Deadline, Done, Err, Value)
type backgroundCtx struct{ emptyCtx }

// String() определяет строковое представление для отладки и логирования
// Вызывается автоматически при fmt.Printf("%v") или fmt.Println()
func (backgroundCtx) String() string {
	return "context.Background"
}

// todoCtx - структура для временного контекста-заглушки
// Аналогично встраивает emptyCtx и наследует его поведение
type todoCtx struct{ emptyCtx }

// String() определяет строковое представление для отладки и логирования
// Вызывается автоматически при fmt.Printf("%v") или fmt.Println()
func (todoCtx) String() string {
	return "context.TODO"
}

// Background возвращает новый пустой контекст без дедлайна и отмены
// Этот контекст никогда не отменяется, не имеет значений и дедлайна
// Используется как корневой контекст для создания производных контекстов
func Background() Context {
	return backgroundCtx{}
	// Возвращаемый тип Context (интерфейс), но фактически backgroundCtx
}

// TODO возвращает пустой контекст-заглушку
// Используется как временное решение, когда непонятно какой контекст использовать
// Или когда функция еще не принимает контекст, но скоро будет доработана
func TODO() Context {
	return todoCtx{}
	// Аналогично возвращается как интерфейс Context
}

// CancelFunc - это функция, которая сообщает операции прекратить работу.
// CancelFunc не ожидает остановки работы (неблокирующая).
// CancelFunc может вызываться из нескольких горутин одновременно.
// После первого вызова последующие вызовы CancelFunc ничего не делают.
type CancelFunc func()

// WithCancel создает производный контекст, который ссылается на родительский контекст,
// но имеет новый канал Done. Канал Done возвращаемого контекста закрывается:
// 1. При вызове возвращаемой функции cancel
// 2. Или при закрытии канала Done родительского контекста
// В зависимости от того, что произойдет раньше.
//
// Отмена этого контекста освобождает связанные с ним ресурсы,
// поэтому код должен вызывать cancel как только операции в этом Context завершены.
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	// Создаем cancelCtx и связываем его с родительским контекстом
	c := withCancel(parent)

	// Возвращаем контекст и функцию-обертку для его отмены
	return c,
		func() {
			// true - удалять из родительского контекста
			// Canceled - тип ошибки (обычная отмена)
			// nil - нет конкретной причины отмены
			c.cancel(true, Canceled, nil)
		}
}

// CancelCauseFunc ведет себя как CancelFunc, но дополнительно устанавливает причину отмены.
// Эту причину можно получить, вызвав Cause на отмененном Context или любом его производном.
//
// Если контекст уже был отменен, CancelCauseFunc не устанавливает причину.
// Пример: если childContext создан из parentContext:
//   - если parentContext отменен с cause1 до того, как childContext отменен с cause2,
//     тогда Cause(parentContext) == Cause(childContext) == cause1
//   - если childContext отменен с cause2 до parentContext отменен с cause1,
//     тогда Cause(parentContext) == cause1 и Cause(childContext) == cause2
type CancelCauseFunc func(cause error)

// WithCancelCause работает как WithCancel, но возвращает CancelCauseFunc вместо CancelFunc.
// Вызов cancel с ненулевой ошибкой (причиной) записывает эту ошибку в ctx;
// затем ее можно получить через Cause(ctx).
// Вызов cancel с nil устанавливает причину как Canceled.
//
// Пример использования:
//
//	ctx, cancel := context.WithCancelCause(parent)
//	cancel(myError)
//	ctx.Err() // возвращает context.Canceled
//	context.Cause(ctx) // возвращает myError
func WithCancelCause(parent Context) (ctx Context, cancel CancelCauseFunc) {
	// Создаем cancelCtx
	c := withCancel(parent)
	// Возвращаем контекст и функцию, которая принимает причину отмены
	return c,
		func(cause error) {
			// true - удалять из родительского контекста
			// Canceled - тип ошибки
			// cause - конкретная причина отмены (может быть nil)
			c.cancel(true, Canceled, cause)
		}
}

// withCancel создает cancelCtx и связывает его с родительским контекстом
func withCancel(parent Context) *cancelCtx {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := &cancelCtx{}

	// Связываем с родительским контекстом:
	// 1. Если родитель уже отменен - сразу отменяем новый контекст
	// 2. Иначе добавляем новый контекст как дочерний к родителю
	c.propagateCancel(parent, c)
	return c
}

// Cause возвращает ненулевую ошибку, объясняющую почему контекст c был отменен.
// Первая отмена c или одного из его родителей устанавливает причину.
// Если отмена произошла через вызов CancelCauseFunc(err), то Cause возвращает err.
// Иначе Cause(c) возвращает то же значение, что и c.Err().
// Cause возвращает nil если c еще не был отменен.
func Cause(c Context) error {
	// Пытаемся получить cancelCtx через ключ cancelCtxKey
	// Это работает только для контекстов, созданных through WithCancel/WithCancelCause
	if cc, ok := c.Value(&cancelCtxKey).(*cancelCtx); ok {
		cc.mu.Lock()      // локаемся т.к. горутины могут обращаться
		cause := cc.cause // Получаем причину отмены
		cc.mu.Unlock()    // анлок

		// Если причина установлена - возвращаем ее
		if cause != nil { // если это CancelCauseFunc, то возвращаем Cause
			return cause
		}
		// Либо контекст не отменен,
		// либо отмена произошла в кастомной реализации контекста, а не в *cancelCtx

	}

	// Нет cancelCtxKey со значением причины, значит c не является
	// потомком отмененного Context созданного через WithCancelCause
	// Возвращаем стандартную ошибку контекста (или nil, если не отменен)
	return c.Err()
}

// AfterFunc планирует вызов функции f в отдельной горутине после отмены ctx.
// Если ctx уже отменен, AfterFunc немедленно вызывает f в отдельной горутине.
//
// Множественные вызовы AfterFunc на одном контексте работают независимо;
// один вызов не заменяет другой.
//
// Вызов возвращаемой функции stop останавливает связь ctx с f.
// Она возвращает true, если вызов остановил выполнение f.
// Если stop возвращает false, то либо контекст уже отменен и f уже запущена,
// либо f уже была остановлена ранее.
// Функция stop не ждет завершения f перед возвратом.
// Если вызывающему нужно знать, завершена ли f,
// он должен координироваться с f явно.
//
// Если ctx имеет метод "AfterFunc(func()) func() bool",
// AfterFunc будет использовать его для планирования вызова.
func AfterFunc(ctx Context, f func()) (stop func() bool) {
	// Создаем afterFuncCtx, который оборачивает функцию f
	a := &afterFuncCtx{
		f: f,
	}

	// Связываем этот afterFuncCtx с родительским контекстом
	// Это добавляет a в children родительского контекста
	a.cancelCtx.propagateCancel(ctx, a)

	// Возвращаем функцию stop, которую можно вызвать,
	// чтобы отменить выполнение f (если еще не поздно)
	return func() bool {
		stopped := false

		// Используем sync.Once для гарантии,
		// что функция выполнится только один раз
		a.once.Do(func() {
			stopped = true
		})
		if stopped {
			// Отменяем afterFuncCtx, что удалит его из родительского списка children
			a.cancel(true, Canceled, nil)
		}
		return stopped
	}
}

// afterFuncer - интерфейс, который может быть реализован кастомными контекстами
// для предоставления собственной оптимизированной реализации AfterFunc
type afterFuncer interface {
	AfterFunc(func()) func() bool
}

// afterFuncCtx - специальный контекст для выполнения функции после отмены
type afterFuncCtx struct {
	cancelCtx           // Встраиваем cancelCtx для наследования всех его полей и методов
	once      sync.Once // Гарантирует однократное выполнение функции f
	f         func()    // Функция, которую нужно выполнить после отмены
}

// cancel - переопределенный метод отмены для afterFuncCtx
func (a *afterFuncCtx) cancel(removeFromParent bool, err, cause error) {
	// Сначала отменяем встроенный cancelCtx
	// false - не удаляем из родителя здесь (сделаем это позже если нужно)
	a.cancelCtx.cancel(false, err, cause)

	// Если требуется, удаляем себя из списка детей родительского контекста
	if removeFromParent {
		removeChild(a.Context, a)
	}
	// Запускаем функцию f, но только один раз (благодаря sync.Once)
	a.once.Do(func() {
		go a.f() // Запускаем в отдельной горутине, чтобы не блокировать
	})
}

// stopCtx используется как родительский контекст для cancelCtx,
// когда AfterFunc был зарегистрирован у родителя.
// Он хранит stop-функцию, используемую для отмены регистрации AfterFunc.
type stopCtx struct {
	Context             // Встраиваем родительский контекст
	stop    func() bool // Функция для остановки AfterFunc
}

// goroutines подсчитывает количество созданных горутин (для тестирования)
var goroutines atomic.Int32

// &cancelCtxKey - ключ, по которому cancelCtx возвращает сам себя
var cancelCtxKey int

// parentCancelCtx возвращает базовый *cancelCtx для родительского контекста
// Она ищет parent.Value(&cancelCtxKey), чтобы найти самый внутренний *cancelCtx
// и проверяет, совпадает ли parent.Done() с этим *cancelCtx.
// (Если нет, значит *cancelCtx обернут в кастомную реализацию,
// предоставляющую другой канал done, и мы не должны его обходить.)
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	// Получаем канал Done родительского контекста
	done := parent.Done()
	// Если канал уже закрыт или nil, это не cancelCtx
	if done == closedchan || done == nil {
		return nil, false
	}
	// Пытаемся получить cancelCtx через Value с ключом cancelCtxKey
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok {
		return nil, false
	}
	// Получаем канал Done из найденного cancelCtx
	pdone, _ := p.done.Load().(chan struct{})
	// Сравниваем каналы Done
	if pdone != done {
		// Каналы разные - значит cancelCtx обернут в другую реализацию
		// Нельзя использовать внутренний cancelCtx напрямую
		return nil, false
	}
	// Все проверки пройдены, возвращаем cancelCtx
	return p, true
}

// removeChild удаляет контекст-ребенок из его родительского контекста
func removeChild(parent Context, child canceler) {
	// Проверяем, является ли родитель stopCtx
	if s, ok := parent.(stopCtx); ok {
		// Если да, вызываем stop-функцию для отмены AfterFunc
		s.stop()
		return
	}
	// Пытаемся получить родительский cancelCtx
	p, ok := parentCancelCtx(parent)
	if !ok {
		// Не смогли найти cancelCtx - ничего не делаем
		return
	}

	// Блокируем доступ к children родительского cancelCtx
	p.mu.Lock()

	// Если у родителя есть дети, удаляем child из списка
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// canceler - это интерфейс для типов контекстов, которые можно отменить напрямую.
// Реализуют этот интерфейс *cancelCtx и *timerCtx.
type canceler interface {
	// cancel отменяет контекст
	// removeFromParent: нужно ли удалять этот контекст из списка детей родителя
	// err: тип ошибки (Canceled или DeadlineExceeded)
	// cause: конкретная причина отмены (может быть nil)
	cancel(removeFromParent bool, err, cause error)
	// Done возвращает канал, который закрывается при отмене контекста
	Done() <-chan struct{}
}

// closedchan - переиспользуемый закрытый канал.
// Создается один раз при инициализации пакета.
var closedchan = make(chan struct{})

// init - функция инициализации пакета, вызывается автоматически
func init() {
	close(closedchan) // Закрываем канал один раз при старте программы
}

// cancelCtx - контекст, который можно отменить.
// При отмене он также отменяет все дочерние контексты,
// реализующие интерфейс canceler.
type cancelCtx struct {
	// Встраиваем интерфейс Context (обычно это родительский контекст)
	// Это позволяет cancelCtx реализовывать интерфейс Context и
	// делегировать методы родителю, если они не переопределены в cancelCtx
	Context

	// mu - мьютекс для защиты следующих полей от одновременного доступа
	// Контексты могут использоваться из нескольких горутин одновременно,
	// поэтому нужна синхронизация
	mu sync.Mutex // protects following fields

	// done - атомарное значение, хранящее канал chan struct{}
	// Канал создается лениво (при первом вызове Done())
	// и закрывается при первом вызове cancel()
	done atomic.Value // of chan struct{}, created lazily, closed by first cancel call

	// children - множество дочерних контекстов, которые можно отменить
	// Хранится как map[canceler]struct{} для быстрого удаления
	// При первой отмене устанавливается в nil, чтобы освободить память
	// и предотвратить дальнейшие изменения
	children map[canceler]struct{} // set to nil by the first cancel call

	// err - атомарное значение, хранящее ошибку отмены
	// Устанавливается в ненулевое значение при первой отмене
	// Используется atomic.Value для потокобезопасного чтения без блокировки
	err atomic.Value // set to non-nil by the first cancel call

	// cause - конкретная причина отмены (для WithCancelCause)
	// Хранится отдельно от err, чтобы можно было различать
	// стандартную отмену и отмену с причиной
	// Защищается мьютексом mu (не atomic.Value, так как error - интерфейс)
	cause error // set to non-nil by the first cancel call
}

func (c *cancelCtx) Value(key any) any {
	// Проверяем, не ищем ли мы сам cancelCtx по специальному ключу
	if key == &cancelCtxKey {
		return c // Возвращаем сам cancelCtx
	}

	// Иначе делегируем поиск родительскому контексту
	return value(c.Context, key)
}

func (c *cancelCtx) Done() <-chan struct{} {
	d := c.done.Load()
	if d != nil {
		return d.(chan struct{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d = c.done.Load()
	if d == nil {
		d = make(chan struct{})
		c.done.Store(d)
	}
	return d.(chan struct{})
}

func (c *cancelCtx) Err() error {
	// An atomic load is ~5x faster than a mutex, which can matter in tight loops.
	if err := c.err.Load(); err != nil {
		// Ensure the done channel has been closed before returning a non-nil error.
		<-c.Done()
		return err.(error)
	}
	return nil
}

// propagateCancel arranges for child to be canceled when parent is.
// It sets the parent context of cancelCtx.
func (c *cancelCtx) propagateCancel(parent Context, child canceler) {
	c.Context = parent

	done := parent.Done()
	if done == nil {
		return // parent is never canceled
	}

	select {
	case <-done:
		// parent is already canceled
		child.cancel(false, parent.Err(), Cause(parent))
		return
	default:
	}

	if p, ok := parentCancelCtx(parent); ok {
		// parent is a *cancelCtx, or derives from one.
		p.mu.Lock()
		if err := p.err.Load(); err != nil {
			// parent has already been canceled
			child.cancel(false, err.(error), p.cause)
		} else {
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
		return
	}

	if a, ok := parent.(afterFuncer); ok {
		// parent implements an AfterFunc method.
		c.mu.Lock()
		stop := a.AfterFunc(func() {
			child.cancel(false, parent.Err(), Cause(parent))
		})
		c.Context = stopCtx{
			Context: parent,
			stop:    stop,
		}
		c.mu.Unlock()
		return
	}

	goroutines.Add(1)
	go func() {
		select {
		case <-parent.Done():
			child.cancel(false, parent.Err(), Cause(parent))
		case <-child.Done():
		}
	}()
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
// cancel sets c.cause to cause if this is the first time c is canceled.
func (c *cancelCtx) cancel(removeFromParent bool, err, cause error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	if cause == nil {
		cause = err
	}
	c.mu.Lock()
	if c.err.Load() != nil {
		c.mu.Unlock()
		return // already canceled
	}
	c.err.Store(err)
	c.cause = cause
	d, _ := c.done.Load().(chan struct{})
	if d == nil {
		c.done.Store(closedchan)
	} else {
		close(d)
	}
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err, cause)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent {
		removeChild(c.Context, c)
	}
}

// WithoutCancel returns a derived context that points to the parent context
// and is not canceled when parent is canceled.
// The returned context returns no Deadline or Err, and its Done channel is nil.
// Calling [Cause] on the returned context returns nil.
func WithoutCancel(parent Context) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	return withoutCancelCtx{parent}
}

type withoutCancelCtx struct {
	c Context
}

func (withoutCancelCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (withoutCancelCtx) Done() <-chan struct{} {
	return nil
}

func (withoutCancelCtx) Err() error {
	return nil
}

func (c withoutCancelCtx) Value(key any) any {
	return value(c, key)
}

func (c withoutCancelCtx) String() string {
	return contextName(c.c) + ".WithoutCancel"
}

// WithDeadline returns a derived context that points to the parent context
// but has the deadline adjusted to be no later than d. If the parent's
// deadline is already earlier than d, WithDeadline(parent, d) is semantically
// equivalent to parent. The returned [Context.Done] channel is closed when
// the deadline expires, when the returned cancel function is called,
// or when the parent context's Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this [Context] complete.
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	return WithDeadlineCause(parent, d, nil)
}

// WithDeadlineCause behaves like [WithDeadline] but also sets the cause of the
// returned Context when the deadline is exceeded. The returned [CancelFunc] does
// not set the cause.
func WithDeadlineCause(parent Context, d time.Time, cause error) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}
	c := &timerCtx{
		deadline: d,
	}
	c.cancelCtx.propagateCancel(parent, c)
	dur := time.Until(d)
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded, cause) // deadline has already passed
		return c, func() { c.cancel(false, Canceled, nil) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err.Load() == nil {
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded, cause)
		})
	}
	return c, func() { c.cancel(true, Canceled, nil) }
}

// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu.

	deadline time.Time
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

func (c *timerCtx) cancel(removeFromParent bool, err, cause error) {
	c.cancelCtx.cancel(false, err, cause)
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this [Context] complete:
//
//	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
//		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
//		defer cancel()  // releases resources if slowOperation completes before timeout elapses
//		return slowOperation(ctx)
//	}
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithTimeoutCause behaves like [WithTimeout] but also sets the cause of the
// returned Context when the timeout expires. The returned [CancelFunc] does
// not set the cause.
func WithTimeoutCause(parent Context, timeout time.Duration, cause error) (Context, CancelFunc) {
	return WithDeadlineCause(parent, time.Now().Add(timeout), cause)
}

// WithValue returns a derived context that points to the parent Context.
// In the derived context, the value associated with key is val.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The provided key must be comparable and should not be of type
// string or any other built-in type to avoid collisions between
// packages using context. Users of WithValue should define their own
// types for keys. To avoid allocating when assigning to an
// interface{}, context keys often have concrete type
// struct{}. Alternatively, exported context key variables' static
// type should be a pointer or interface.
func WithValue(parent Context, key, val any) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
type valueCtx struct {
	Context
	key, val any
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v any) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	case nil:
		return "<nil>"
	}
	return reflectlite.TypeOf(v).String()
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(" +
		stringify(c.key) + ", " +
		stringify(c.val) + ")"
}

func (c *valueCtx) Value(key any) any {
	if c.key == key {
		return c.val
	}
	return value(c.Context, key)
}

func value(c Context, key any) any {
	for {
		switch ctx := c.(type) {
		case *valueCtx:
			if key == ctx.key {
				return ctx.val
			}
			c = ctx.Context
		case *cancelCtx:
			if key == &cancelCtxKey {
				return c
			}
			c = ctx.Context
		case withoutCancelCtx:
			if key == &cancelCtxKey {
				// This implements Cause(ctx) == nil
				// when ctx is created using WithoutCancel.
				return nil
			}
			c = ctx.c
		case *timerCtx:
			if key == &cancelCtxKey {
				return &ctx.cancelCtx
			}
			c = ctx.Context
		case backgroundCtx, todoCtx:
			return nil
		default:
			return c.Value(key)
		}
	}
}
