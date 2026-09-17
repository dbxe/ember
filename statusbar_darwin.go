//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>
#import <stdatomic.h>

static NSStatusItem *statusItem = nil;
static NSMenu *statusMenu = nil;
static atomic_int lastClickedTag = ATOMIC_VAR_INIT(-1);
static atomic_int statusMenuOpen = ATOMIC_VAR_INIT(0);

static NSImage *emberStatusImage(void) {
    if (@available(macOS 11.0, *)) {
        NSImage *symbol = [NSImage imageWithSystemSymbolName:@"flame.fill"
                                    accessibilityDescription:@"Ember"];
        if (symbol != nil) {
            NSImageSymbolConfiguration *config =
                [NSImageSymbolConfiguration configurationWithPointSize:12.0
                                                                weight:NSFontWeightLight
                                                                 scale:NSImageSymbolScaleSmall];
            if (@available(macOS 12.0, *)) {
                config = [config configurationByApplyingConfiguration:
                    [NSImageSymbolConfiguration configurationWithHierarchicalColor:[NSColor whiteColor]]];
            }
            symbol = [symbol imageWithSymbolConfiguration:config];
            [symbol setTemplate:NO];
            return symbol;
        }
    }

    NSImage *image = [[NSImage alloc] initWithSize:NSMakeSize(14.0, 14.0)];
    [image lockFocus];
    [[NSColor whiteColor] setStroke];
    NSBezierPath *ring = [NSBezierPath bezierPathWithOvalInRect:NSMakeRect(4.0, 4.0, 6.0, 6.0)];
    [ring setLineWidth:1.2];
    [ring stroke];
    [image unlockFocus];
    [image setTemplate:NO];
    return [image autorelease];
}

@interface StatusBarDelegate : NSObject <NSMenuDelegate>
- (void)menuItemClicked:(id)sender;
- (void)menuWillOpen:(NSMenu *)menu;
- (void)menuDidClose:(NSMenu *)menu;
- (void)setupStatusBar:(NSString *)title;
@end

@implementation StatusBarDelegate

- (void)menuItemClicked:(id)sender {
    atomic_store(&lastClickedTag, (int)[sender tag]);
}

- (void)menuWillOpen:(NSMenu *)menu {
    atomic_store(&statusMenuOpen, 1);
}

- (void)menuDidClose:(NSMenu *)menu {
    atomic_store(&statusMenuOpen, 0);
}

- (void)setupStatusBar:(NSString *)title {
    if (statusItem != nil) return;

	statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
	[statusItem retain];
	statusItem.button.title = @"";
	statusItem.button.image = emberStatusImage();
	statusItem.button.imagePosition = NSImageOnly;
	statusItem.button.toolTip = title;
	[statusItem.button setAccessibilityLabel:title];

	statusMenu = [[NSMenu alloc] init];
	statusMenu.delegate = self;
	statusItem.menu = statusMenu;
}

@end

static StatusBarDelegate *delegate = nil;

void bootstrapApplication() {
    [NSApplication sharedApplication];
}

void setAccessoryActivationPolicy() {
    [[NSApplication sharedApplication] setActivationPolicy:NSApplicationActivationPolicyAccessory];
}

void runApplication() {
    [[NSApplication sharedApplication] setActivationPolicy:NSApplicationActivationPolicyAccessory];
    [NSApp run];
}

void terminateApplication() {
    dispatch_async(dispatch_get_main_queue(), ^{
        [NSApp terminate:nil];
    });
}

void initStatusBarOnMainThread(const char *title) {
    if (delegate == nil) {
        delegate = [[StatusBarDelegate alloc] init];
    }

    NSString *nsTitle = [NSString stringWithUTF8String:title];
    if ([NSThread isMainThread]) {
        [delegate setupStatusBar:nsTitle];
    } else {
        dispatch_sync(dispatch_get_main_queue(), ^{
            [delegate setupStatusBar:nsTitle];
        });
    }
}

void setStatusTitleOnMainThread(const char *title) {
    if (statusItem == nil) return;
    NSString *nsTitle = [NSString stringWithUTF8String:title];
    if ([NSThread isMainThread]) {
        statusItem.button.toolTip = nsTitle;
        [statusItem.button setAccessibilityLabel:nsTitle];
    } else {
        dispatch_async(dispatch_get_main_queue(), ^{
            statusItem.button.toolTip = nsTitle;
            [statusItem.button setAccessibilityLabel:nsTitle];
        });
    }
}

void clearMenuOnMainThread() {
    if (statusMenu == nil) return;
    if ([NSThread isMainThread]) {
        [statusMenu removeAllItems];
    } else {
        dispatch_sync(dispatch_get_main_queue(), ^{
            [statusMenu removeAllItems];
        });
    }
}

void addMenuItemOnMainThread(const char *title, int tag, int enabled, int checked) {
    if (statusMenu == nil || delegate == nil) return;

    NSString *nsTitle = [NSString stringWithUTF8String:title];
    void (^block)(void) = ^{
        NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:nsTitle
                                                      action:@selector(menuItemClicked:)
                                               keyEquivalent:@""];
        [item setTarget:delegate];
        [item setTag:tag];
        [item setEnabled:enabled != 0];
        [item setState:checked != 0 ? NSControlStateValueOn : NSControlStateValueOff];
        [statusMenu addItem:item];
    };

    if ([NSThread isMainThread]) {
        block();
    } else {
        dispatch_sync(dispatch_get_main_queue(), block);
    }
}

void addSeparatorOnMainThread() {
    if (statusMenu == nil) return;
    if ([NSThread isMainThread]) {
        [statusMenu addItem:[NSMenuItem separatorItem]];
    } else {
        dispatch_sync(dispatch_get_main_queue(), ^{
            [statusMenu addItem:[NSMenuItem separatorItem]];
        });
    }
}

int statusMenuIsAttached() {
    return atomic_load(&statusMenuOpen);
}

int pollClickedTag() {
    return atomic_exchange(&lastClickedTag, -1);
}

void cleanupStatusBar() {
    void (^block)(void) = ^{
        if (statusItem != nil) {
            [[NSStatusBar systemStatusBar] removeStatusItem:statusItem];
            [statusItem release];
            statusItem = nil;
        }
    };

    if ([NSThread isMainThread]) {
        block();
    } else {
        dispatch_async(dispatch_get_main_queue(), block);
    }
}
*/
import "C"
import (
	"time"
	"unsafe"
)

const (
	TagHeader = iota + 1
	TagRefresh
	TagQuit
)

const (
	TagAccountBase = 1000
	TagDetailsBase = 2000
)

var statusBarApp *App

func SetupStatusBar(app *App) {
	statusBarApp = app

	C.bootstrapApplication()
	C.setAccessoryActivationPolicy()

	title := C.CString("Ember")
	C.initStatusBarOnMainThread(title)
	C.free(unsafe.Pointer(title))

	go clickLoop()
}

func clickLoop() {
	for {
		select {
		case <-statusBarApp.stopCh:
			return
		case <-timeAfter(100 * time.Millisecond):
			tag := int(C.pollClickedTag())
			if tag >= 0 && statusBarApp != nil {
				statusBarApp.HandleMenuAction(tag)
			}
			if statusBarApp != nil && !StatusMenuIsOpen() {
				statusBarApp.rebuildMenuIfNeeded()
			}
		}
	}
}

func SetStatusTitle(title string) {
	cTitle := C.CString(title)
	defer C.free(unsafe.Pointer(cTitle))
	C.setStatusTitleOnMainThread(cTitle)
}

func ClearMenu() {
	C.clearMenuOnMainThread()
}

func AddSeparator() {
	C.addSeparatorOnMainThread()
}

func StatusMenuIsOpen() bool {
	return C.statusMenuIsAttached() != 0
}

func addMenuItem(title string, tag int, enabled bool, checked bool) {
	cTitle := C.CString(title)
	defer C.free(unsafe.Pointer(cTitle))

	var e C.int
	if enabled {
		e = 1
	}
	var state C.int
	if checked {
		state = 1
	}
	C.addMenuItemOnMainThread(cTitle, C.int(tag), e, state)
}

func RemoveStatusBar() {
	C.cleanupStatusBar()
}

func RunApp() {
	C.runApplication()
}

func TerminateApp() {
	C.terminateApplication()
}

func timeAfter(d time.Duration) <-chan time.Time {
	return time.After(d)
}
