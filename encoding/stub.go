// +build windows nacl plan9

// Copyright 2015 The TCell Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package encoding

func Register() {
	// So Windows is only UTF-16LE (yay!)

	// Other platforms that don't use termios/terminfo are pretty much unsupported.
	// Therefore, we shouldn't bring in all this stuff because it creates a lot of
	// bloat for those platforms.  So, just punt.
}
